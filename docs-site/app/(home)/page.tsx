import Link from 'next/link';
import { ArrowRight, Flame, Gauge, Layers, Syringe } from 'lucide-react';
import { Timeline } from '@/components/timeline';
import { blogUrl, repoUrl } from '@/lib/shared';

const features = [
  {
    icon: Flame,
    title: 'A flame graph per span',
    text: 'Open a trace in Grafana, click a span, and see the CUDA kernels it ran and how long each took on the GPU.',
    href: '/docs/how-it-works',
  },
  {
    icon: Syringe,
    title: 'No code changes',
    text: 'CUDA loads the capture library into the process when one environment variable is set.',
    href: '/docs/quickstart',
  },
  {
    icon: Layers,
    title: 'Five languages',
    text: 'Small span bridges for Python, C++, Go, Java and Rust tell the library which span is current.',
    href: '/docs/bridges',
  },
  {
    icon: Gauge,
    title: 'Made to stay on',
    text: 'No locks or allocations on the launch path. When something is full, records are dropped and counted.',
    href: '/docs/limitations',
  },
];

const steps = [
  {
    n: '01',
    title: 'Build',
    text: 'The capture library against your CUDA toolkit, and the Go agent.',
    code: 'make',
  },
  {
    n: '02',
    title: 'Start the agent',
    text: 'One per node. It sums kernel time per process, span and kernel, and exports OTLP profiles.',
    code: './bin/warpspan --otlp-endpoint …',
  },
  {
    n: '03',
    title: 'Inject',
    text: 'Set the variable on your service. The span bridge loads with it.',
    code: 'CUDA_INJECTION64_PATH=…/libwarpspan.so',
  },
];

const bridges = [
  { name: 'Python', how: 'loads by itself (sitecustomize or opentelemetry-instrument)', href: '/docs/bridges/python' },
  { name: 'C++', how: 'one Install() call at startup', href: '/docs/bridges/cpp' },
  { name: 'Go', how: 'wrap cgo GPU calls in Do(ctx, fn)', href: '/docs/bridges/go' },
  { name: 'Java', how: 'Warpspan.install() at startup, Java 22+', href: '/docs/bridges/java' },
  { name: 'Rust', how: 'a tracing layer after tracing-opentelemetry', href: '/docs/bridges/rust' },
];

export default function HomePage() {
  return (
    <main className="flex flex-1 flex-col">
      <section className="border-b border-fd-border">
        <div className="mx-auto grid w-full max-w-6xl gap-12 px-6 pb-16 pt-14 md:grid-cols-[1fr_1fr] md:items-center md:pb-24 md:pt-20">
          <div className="min-w-0 space-y-7">
            <p className="font-ui inline-flex items-center gap-2 border border-fd-border bg-fd-card px-3 py-1 text-xs text-fd-muted-foreground">
              <span className="size-1.5 bg-fd-primary" />
              Prototype · Linux · NVIDIA GPUs
            </p>
            <h1 className="text-[2.6rem] font-bold leading-[1.04] tracking-tight md:text-[3.5rem]">
              GPU time,
              <br />
              <span className="text-fd-primary">per request.</span>
            </h1>
            <p className="max-w-xl text-lg leading-relaxed text-fd-muted-foreground">
              Warpspan records every CUDA kernel your service runs and tags it
              with the OpenTelemetry span that launched it. Open a slow request
              in Grafana and see which kernels it spent its GPU time on.
            </p>
            <div className="font-ui flex flex-wrap items-center gap-x-6 gap-y-3 text-sm">
              <Link
                href="/docs/quickstart"
                className="inline-flex items-center gap-2 bg-fd-primary px-4 py-2.5 font-medium text-fd-primary-foreground transition-opacity hover:opacity-90"
              >
                Get started
                <ArrowRight className="size-4" />
              </Link>
              <Link
                href="/docs/how-it-works"
                className="text-fd-muted-foreground underline-offset-4 transition-colors hover:text-fd-foreground hover:underline"
              >
                How it works
              </Link>
              <a
                href={repoUrl}
                className="text-fd-muted-foreground underline-offset-4 transition-colors hover:text-fd-foreground hover:underline"
              >
                GitHub
              </a>
            </div>
          </div>
          <Timeline className="mx-auto w-full max-w-[520px] text-fd-foreground" />
        </div>
      </section>

      <section className="mx-auto w-full max-w-6xl px-6 py-16 md:py-20">
        <h2 className="font-ui mb-2 text-sm font-medium uppercase tracking-[0.14em] text-fd-muted-foreground">
          What you get
        </h2>
        <p className="mb-10 max-w-2xl text-2xl leading-snug">
          The GPU work behind each span in your traces, in the Grafana you already use.
        </p>
        <div className="grid gap-px overflow-hidden border border-fd-border bg-fd-border sm:grid-cols-2 lg:grid-cols-4">
          {features.map(item => (
            <Link
              key={item.title}
              href={item.href}
              className="group flex flex-col gap-3 bg-fd-background p-6 transition-colors hover:bg-fd-card"
            >
              <item.icon className="size-5 text-fd-primary" strokeWidth={1.75} />
              <h3 className="text-base font-semibold">{item.title}</h3>
              <p className="flex-1 text-sm leading-relaxed text-fd-muted-foreground">{item.text}</p>
            </Link>
          ))}
        </div>
      </section>

      <section className="border-y border-fd-border bg-fd-card/50">
        <div className="mx-auto grid w-full max-w-6xl gap-10 px-6 py-16 md:grid-cols-3 md:py-20">
          {steps.map(step => (
            <div key={step.n} className="space-y-3">
              <span className="font-ui text-sm text-fd-primary">{step.n}</span>
              <h3 className="text-lg font-semibold">{step.title}</h3>
              <p className="text-sm leading-relaxed text-fd-muted-foreground">{step.text}</p>
              <code className="inline-block border border-fd-border bg-fd-background px-2.5 py-1 font-mono text-xs text-fd-muted-foreground">
                {step.code}
              </code>
            </div>
          ))}
        </div>
      </section>

      <section className="mx-auto grid w-full max-w-6xl gap-10 px-6 py-16 md:grid-cols-[0.9fr_1.1fr] md:py-20">
        <div className="space-y-4">
          <h2 className="font-ui text-sm font-medium uppercase tracking-[0.14em] text-fd-muted-foreground">
            Span bridges
          </h2>
          <p className="text-2xl leading-snug">
            A small bridge per language tells the capture library which span is
            current on each thread.
          </p>
          <Link
            href="/docs/bridges"
            className="font-ui inline-flex items-center gap-2 text-sm text-fd-primary underline-offset-4 hover:underline"
          >
            How bridges work
            <ArrowRight className="size-4" />
          </Link>
        </div>
        <ul className="grid gap-px border border-fd-border bg-fd-border">
          {bridges.map(b => (
            <li key={b.name}>
              <Link
                href={b.href}
                className="flex items-baseline justify-between gap-4 bg-fd-background px-5 py-4 transition-colors hover:bg-fd-card"
              >
                <span className="font-semibold">{b.name}</span>
                <span className="text-right text-sm text-fd-muted-foreground">{b.how}</span>
              </Link>
            </li>
          ))}
        </ul>
      </section>

      <footer className="mt-auto border-t border-fd-border">
        <div className="font-ui mx-auto flex w-full max-w-6xl flex-wrap items-center justify-between gap-4 px-6 py-8 text-xs text-fd-muted-foreground">
          <span>Warpspan by Razin</span>
          <nav className="flex gap-6">
            <Link href="/docs" className="hover:text-fd-foreground">
              Docs
            </Link>
            <a href={blogUrl} className="hover:text-fd-foreground">
              Blog post
            </a>
            <a href={repoUrl} className="hover:text-fd-foreground">
              GitHub
            </a>
          </nav>
        </div>
      </footer>
    </main>
  );
}
