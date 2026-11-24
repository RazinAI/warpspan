//! Warpspan span bridge for Rust services that use `tracing` with
//! `tracing-opentelemetry`.
//!
//! libwarpspan.so keeps the current span per OS thread and tags the CUDA
//! kernels launched on that thread with it. This crate provides a `tracing`
//! layer that publishes the current OpenTelemetry span whenever a `tracing`
//! span is entered or exited. Instrumented futures (`.instrument()`,
//! `#[instrument]`) enter their span on every poll, so they are attributed per
//! poll; work moved to `spawn_blocking`, rayon or un-instrumented tasks is not.
//!
//! Add it after the OpenTelemetry layer, which must keep context activation on
//! (the default): the layer reads `opentelemetry::Context::current()`, which
//! that layer sets on enter. Both crates must use the same (semver-compatible)
//! `opentelemetry` version, or they see different current contexts. These
//! mistakes can't be detected up front; if the layer sees no OpenTelemetry span
//! in its first 10,000 span enters, it prints one warning to stderr.
//!
//! ```ignore
//! let warpspan = match warpspan_otel::layer() {
//!     Ok(layer) => Some(layer),
//!     Err(warpspan_otel::Error::NotConfigured) => None, // warpspan isn't in use
//!     Err(e) => { eprintln!("{e}"); None }
//! };
//! let subscriber = tracing_subscriber::registry()
//!     .with(tracing_opentelemetry::layer().with_tracer(tracer))
//!     .with(warpspan);
//! ```
//!
//! Plain `opentelemetry::Context::attach` without a `tracing` span isn't seen.
//! opentelemetry 0.33 has an experimental context observer for this
//! (`experimental_context_observer`), not used here because it is experimental
//! and allows only one global observer.

use std::ffi::{CStr, CString};
use std::fmt;
use std::panic::{AssertUnwindSafe, catch_unwind};
use std::sync::atomic::{AtomicBool, AtomicU32, Ordering};

use opentelemetry::trace::TraceContextExt;
use opentelemetry::{Context, KeyValue};
use tracing::span;
use tracing_subscriber::layer::{self, Layer};
use tracing_subscriber::registry::LookupSpan;

/// Must match WARPSPAN_ABI_VERSION in capture/include/warpspan.h.
const ABI_VERSION: u32 = 1;

/// Span attribute Grafana needs to offer "Profiles for this span".
pub const PROFILE_ID_ATTRIBUTE: &str = "pyroscope.profile.id";

/// Span enters without any OpenTelemetry span before the warning is printed.
const WARN_AFTER_ENTERS: u32 = 10_000;

type SetSpanFn = unsafe extern "C" fn(*const u8, *const u8);

/// Why the layer couldn't be created.
#[derive(Debug)]
pub enum Error {
    /// No path given and CUDA_INJECTION64_PATH is unset: warpspan isn't in use.
    NotConfigured,
    /// The library couldn't be loaded or isn't a compatible libwarpspan.so.
    Load(String),
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Error::NotConfigured => write!(f, "warpspan: CUDA_INJECTION64_PATH is not set"),
            Error::Load(msg) => write!(f, "warpspan: {msg}"),
        }
    }
}

impl std::error::Error for Error {}

/// A `tracing` layer that publishes the current OpenTelemetry span to
/// libwarpspan.so. Only sampled, local spans are published, as in the other
/// bridges.
pub struct WarpspanLayer {
    set_span: SetSpanFn,
    seen_otel_span: AtomicBool,
    enters_without_otel_span: AtomicU32,
}

/// Marks a `tracing` span whose OpenTelemetry span already has the profile ID.
struct Tagged;

/// The layer with the library from `$CUDA_INJECTION64_PATH`.
pub fn layer() -> Result<WarpspanLayer, Error> {
    match std::env::var("CUDA_INJECTION64_PATH") {
        Ok(path) if !path.is_empty() => WarpspanLayer::from_path(&path),
        _ => Err(Error::NotConfigured),
    }
}

impl WarpspanLayer {
    /// Loads libwarpspan.so from `path`. It must be the file CUDA loads
    /// (CUDA_INJECTION64_PATH): dlopen then returns that copy, so both share
    /// one thread-local.
    pub fn from_path(path: &str) -> Result<Self, Error> {
        let cpath =
            CString::new(path).map_err(|_| Error::Load(format!("{path}: path contains NUL")))?;
        // SAFETY: dlopen/dlsym/dlerror with valid C strings. The handle is
        // never closed, so the function pointers stay valid.
        unsafe {
            let handle = libc::dlopen(cpath.as_ptr(), libc::RTLD_NOW | libc::RTLD_LOCAL);
            if handle.is_null() {
                let err = libc::dlerror();
                let msg = if err.is_null() {
                    path.to_string()
                } else {
                    CStr::from_ptr(err).to_string_lossy().into_owned()
                };
                return Err(Error::Load(msg));
            }
            let sym = |name: &CStr| libc::dlsym(handle, name.as_ptr());
            let (abi, set) = (sym(c"warpspan_abi_version"), sym(c"warpspan_set_span"));
            if abi.is_null() || set.is_null() {
                libc::dlclose(handle);
                return Err(Error::Load(format!("{path} is not libwarpspan.so")));
            }
            let abi = std::mem::transmute::<*mut libc::c_void, unsafe extern "C" fn() -> u32>(abi);
            let version = abi();
            if version != ABI_VERSION {
                libc::dlclose(handle);
                return Err(Error::Load(format!(
                    "{path}: ABI version {version}, bridge expects {ABI_VERSION}"
                )));
            }
            Ok(WarpspanLayer {
                set_span: std::mem::transmute::<*mut libc::c_void, SetSpanFn>(set),
                seen_otel_span: AtomicBool::new(false),
                enters_without_otel_span: AtomicU32::new(0),
            })
        }
    }

    /// Publishes the span in cx; if tag, also sets the profile ID on it.
    /// Returns whether cx holds a valid OpenTelemetry span.
    fn publish(&self, cx: &Context, tag: bool) -> bool {
        let span = cx.span();
        let sc = span.span_context();
        if !sc.is_valid() || !sc.is_sampled() || sc.is_remote() {
            // SAFETY: NULL clears the span.
            unsafe { (self.set_span)(std::ptr::null(), std::ptr::null()) };
            return sc.is_valid();
        }
        let (trace_id, span_id) = (sc.trace_id().to_bytes(), sc.span_id().to_bytes());
        // SAFETY: 16- and 8-byte buffers, as warpspan_set_span expects.
        unsafe { (self.set_span)(trace_id.as_ptr(), span_id.as_ptr()) };
        if tag && span.is_recording() {
            span.set_attribute(KeyValue::new(
                PROFILE_ID_ATTRIBUTE,
                sc.span_id().to_string(),
            ));
        }
        true
    }

    /// Warns once if no OpenTelemetry span shows up at all: wrong layer order,
    /// context activation off, or a second opentelemetry version.
    fn note_enter(&self, saw_otel_span: bool) {
        if self.seen_otel_span.load(Ordering::Relaxed) {
            return;
        }
        if saw_otel_span {
            self.seen_otel_span.store(true, Ordering::Relaxed);
        } else if self
            .enters_without_otel_span
            .fetch_add(1, Ordering::Relaxed)
            + 1
            == WARN_AFTER_ENTERS
        {
            eprintln!(
                "warpspan: no OpenTelemetry span in {WARN_AFTER_ENTERS} span enters; add the warpspan layer after \
                 the tracing-opentelemetry layer, keep its context activation on, and use the same opentelemetry version"
            );
        }
    }
}

impl<S> Layer<S> for WarpspanLayer
where
    S: tracing::Subscriber + for<'a> LookupSpan<'a>,
{
    fn on_enter(&self, id: &span::Id, ctx: layer::Context<'_, S>) {
        // Never panic into the app: Context::current() can panic when a span is
        // entered during thread teardown.
        let _ = catch_unwind(AssertUnwindSafe(|| {
            // Tag each span once. Decide under the extensions lock, but release
            // it before touching the OpenTelemetry span: the SDK may log through
            // tracing, which can take the same lock.
            let first = ctx.span(id).is_some_and(|span| {
                let mut ext = span.extensions_mut();
                ext.get_mut::<Tagged>().is_none() && {
                    ext.insert(Tagged);
                    true
                }
            });
            let saw = Context::map_current(|cx| self.publish(cx, first));
            self.note_enter(saw);
        }));
    }

    fn on_exit(&self, _id: &span::Id, _ctx: layer::Context<'_, S>) {
        let _ = catch_unwind(AssertUnwindSafe(|| {
            Context::map_current(|cx| self.publish(cx, false));
        }));
    }
}

#[cfg(test)]
mod tests;
