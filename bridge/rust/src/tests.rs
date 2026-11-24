// Tests run against the real libwarpspan.so (no GPU needed: CUDA never calls
// InitializeInjection here, only the span thread-local is exercised).

use std::future::Future;
use std::pin::pin;
use std::task::{Context as TaskContext, Poll, Waker};

use opentelemetry::trace::{
    SpanContext, SpanId, TraceFlags, TraceId, TraceState, TracerProvider as _,
};
use opentelemetry_sdk::trace::{InMemorySpanExporter, Sampler, SdkTracerProvider, SpanData};
use tracing::Instrument;
use tracing_opentelemetry::OpenTelemetrySpanExt;
use tracing_subscriber::layer::SubscriberExt;

use super::*;

const LIB: &str = concat!(env!("CARGO_MANIFEST_DIR"), "/../../capture/libwarpspan.so");

/// The (trace, span) libwarpspan.so holds for this thread, or None.
fn published() -> Option<(TraceId, SpanId)> {
    let cpath = CString::new(LIB).unwrap();
    // SAFETY: as in from_path; warpspan_get_span takes 16- and 8-byte buffers.
    unsafe {
        let handle = libc::dlopen(cpath.as_ptr(), libc::RTLD_NOW | libc::RTLD_LOCAL);
        assert!(!handle.is_null(), "{LIB} not built (make -C capture)");
        let get = libc::dlsym(handle, c"warpspan_get_span".as_ptr());
        let get = std::mem::transmute::<
            *mut libc::c_void,
            unsafe extern "C" fn(*mut u8, *mut u8) -> i32,
        >(get);
        let (mut trace_id, mut span_id) = ([0u8; 16], [0u8; 8]);
        (get(trace_id.as_mut_ptr(), span_id.as_mut_ptr()) != 0)
            .then(|| (TraceId::from_bytes(trace_id), SpanId::from_bytes(span_id)))
    }
}

/// The OpenTelemetry (trace, span) IDs of a tracing span.
fn ids(span: &tracing::Span) -> Option<(TraceId, SpanId)> {
    let cx = span.context();
    let sc = cx.span().span_context().clone();
    Some((sc.trace_id(), sc.span_id()))
}

fn profile_id_tags(span: &SpanData) -> Vec<String> {
    span.attributes
        .iter()
        .filter(|kv| kv.key.as_str() == PROFILE_ID_ATTRIBUTE)
        .map(|kv| kv.value.as_str().into_owned())
        .collect()
}

/// Runs f with a subscriber: registry + OpenTelemetry layer + warpspan layer.
/// Returns the provider too: dropping it shuts down the exporter, which clears it.
fn with_subscriber(
    sampler: Sampler,
    f: impl FnOnce(),
) -> (InMemorySpanExporter, SdkTracerProvider) {
    let exporter = InMemorySpanExporter::default();
    let provider = SdkTracerProvider::builder()
        .with_sampler(sampler)
        .with_simple_exporter(exporter.clone())
        .build();
    let subscriber = tracing_subscriber::registry()
        .with(tracing_opentelemetry::layer().with_tracer(provider.tracer("test")))
        .with(WarpspanLayer::from_path(LIB).expect("load libwarpspan.so"));
    tracing::subscriber::with_default(subscriber, f);
    (exporter, provider)
}

/// Polls a future to completion with a no-op waker.
fn run(fut: impl Future<Output = ()>) {
    let mut fut = pin!(fut);
    let mut cx = TaskContext::from_waker(Waker::noop());
    while fut.as_mut().poll(&mut cx).is_pending() {}
}

/// A future that is pending once.
struct Yield(bool);

impl Future for Yield {
    type Output = ();
    fn poll(mut self: std::pin::Pin<&mut Self>, _cx: &mut TaskContext<'_>) -> Poll<()> {
        if self.0 {
            Poll::Ready(())
        } else {
            self.0 = true;
            Poll::Pending
        }
    }
}

#[test]
fn load_errors() {
    assert!(matches!(
        WarpspanLayer::from_path("/nonexistent/libwarpspan.so"),
        Err(Error::Load(_))
    ));
    match WarpspanLayer::from_path("libm.so.6") {
        Err(Error::Load(msg)) => assert!(msg.contains("is not libwarpspan.so"), "{msg}"),
        other => panic!("libm accepted: {:?}", other.err()),
    }
}

#[test]
fn nested_spans_publish_and_restore() {
    let (exporter, _provider) = with_subscriber(Sampler::AlwaysOn, || {
        assert_eq!(published(), None);
        let outer = tracing::info_span!("outer");
        {
            let _o = outer.enter();
            assert_eq!(published(), ids(&outer));
            let inner = tracing::info_span!("inner");
            {
                let _i = inner.enter();
                assert_eq!(published(), ids(&inner));
            }
            assert_eq!(published(), ids(&outer));
            for _ in 0..3 {
                let _i = inner.enter(); // re-entering tags once
            }
        }
        assert_eq!(published(), None);
    });
    let spans = exporter.get_finished_spans().unwrap();
    assert_eq!(spans.len(), 2);
    for s in spans {
        assert_eq!(
            profile_id_tags(&s),
            [s.span_context.span_id().to_string()],
            "{}",
            s.name
        );
    }
}

#[test]
fn unsampled_spans_are_not_published() {
    with_subscriber(Sampler::AlwaysOff, || {
        let span = tracing::info_span!("dropped");
        let _s = span.enter();
        assert_eq!(published(), None);
    });
}

#[test]
fn remote_spans_are_not_published() {
    let layer = WarpspanLayer::from_path(LIB).unwrap();
    let remote = SpanContext::new(
        TraceId::from_bytes([1; 16]),
        SpanId::from_bytes([2; 8]),
        TraceFlags::SAMPLED,
        true,
        TraceState::default(),
    );
    assert!(layer.publish(&Context::new().with_remote_span_context(remote), true));
    assert_eq!(published(), None);
}

#[test]
fn async_tasks_are_attributed_per_poll() {
    with_subscriber(Sampler::AlwaysOn, || {
        let span = tracing::info_span!("request");
        let id = ids(&span);
        let mut task = pin!(
            async {
                assert_eq!(published(), id);
                Yield(false).await; // the span is exited and re-entered around this
                assert_eq!(published(), id);
            }
            .instrument(span)
        );
        let mut cx = TaskContext::from_waker(Waker::noop());
        assert_eq!(task.as_mut().poll(&mut cx), Poll::Pending);
        assert_eq!(published(), None, "between polls");
        assert_eq!(task.as_mut().poll(&mut cx), Poll::Ready(()));
    });
}

#[test]
fn nested_async_spans_are_tagged_once() {
    let (exporter, _provider) = with_subscriber(Sampler::AlwaysOn, || {
        let inner = async {
            for _ in 0..50 {
                Yield(false).await;
            }
        }
        .instrument(tracing::info_span!("inner"));
        run(inner.instrument(tracing::info_span!("outer"))); // outer is entered around each inner poll
    });
    let spans = exporter.get_finished_spans().unwrap();
    assert_eq!(spans.len(), 2);
    for s in spans {
        assert_eq!(profile_id_tags(&s).len(), 1, "{}", s.name);
    }
}
