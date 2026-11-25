'use client';

import { useState } from 'react';

// The home page's picture: one request from the PyTorch example. Spans run on
// the CPU; their kernels run later on the GPU, and each kernel stays tied to
// the span that launched it. Hover or tap a span to see its kernels.
// Kernel names are the example's; widths are illustrative, not measured.

type Span = { id: string; label: string; x: number; w: number };
type Kernel = { span: string; name: string; x: number; w: number; launch: number };

const spans: Span[] = [
  { id: 'preprocess', label: 'preprocess', x: 40, w: 110 },
  { id: 'forward', label: 'forward', x: 160, w: 150 },
  { id: 'postprocess', label: 'postprocess', x: 320, w: 110 },
];

const kernels: Kernel[] = [
  { span: 'preprocess', name: 'randn', x: 72, w: 26, launch: 58 },
  { span: 'preprocess', name: 'mean', x: 102, w: 12, launch: 92 },
  { span: 'preprocess', name: 'std', x: 118, w: 14, launch: 120 },
  { span: 'forward', name: 'ampere_sgemm', x: 196, w: 64, launch: 176 },
  { span: 'forward', name: 'gelu', x: 264, w: 16, launch: 226 },
  { span: 'forward', name: 'ampere_sgemm', x: 284, w: 44, launch: 262 },
  { span: 'postprocess', name: 'softmax', x: 360, w: 18, launch: 342 },
  { span: 'postprocess', name: 'topk', x: 382, w: 34, launch: 380 },
];

const LANE = { request: 42, spans: 82, launch: 128, gpu: 206 };

export function Timeline({ className = '' }: { className?: string }) {
  const [active, setActive] = useState<string | null>('forward');
  const shown = kernels.filter(k => k.span === active);

  return (
    <figure className={`m-0 ${className}`}>
      <svg
        viewBox="0 0 470 260"
        className="w-full"
        role="img"
        aria-label="A request's spans on the CPU and the kernels they launched on the GPU, each kernel tied back to its span"
      >
        <g className="font-mono" fontSize="11">
          <text x="0" y={LANE.request + 4} className="fill-fd-muted-foreground">
            trace
          </text>
          <text x="0" y={LANE.launch + 4} className="fill-fd-muted-foreground">
            CPU
          </text>
          <text x="0" y={LANE.gpu + 4} className="fill-fd-muted-foreground">
            GPU
          </text>
        </g>

        <rect x="40" y={LANE.request - 9} width="390" height="18" fill="var(--color-ws-span)" opacity="0.35" />
        <text x="46" y={LANE.request + 4} className="fill-fd-foreground font-mono" fontSize="11">
          POST /predict
        </text>

        <line x1="40" x2="440" y1={LANE.launch} y2={LANE.launch} className="stroke-fd-border" />
        <line x1="40" x2="440" y1={LANE.gpu} y2={LANE.gpu} className="stroke-fd-border" />

        {kernels.map((k, i) => {
          const on = k.span === active;
          return (
            <g key={`${k.name}-${i}`} opacity={active && !on ? 0.28 : 1}>
              <line
                x1={k.launch}
                y1={LANE.launch}
                x2={k.x + 2}
                y2={LANE.gpu - 9}
                stroke="var(--color-ws-kernel)"
                strokeWidth={on ? 1.5 : 1}
                strokeDasharray={on ? undefined : '2 3'}
                className="ws-tie"
                style={{ animationDelay: `${0.5 + i * 0.12}s` }}
              />
              <rect x={k.launch - 1.5} y={LANE.launch - 5} width="3" height="10" className="fill-fd-foreground" />
              <rect
                x={k.x}
                y={LANE.gpu - 9}
                width={k.w}
                height="18"
                fill="var(--color-ws-kernel)"
                className="ws-kernel"
                style={{ animationDelay: `${0.35 + i * 0.12}s` }}
              />
            </g>
          );
        })}

        {spans.map(s => {
          const on = s.id === active;
          return (
            <g
              key={s.id}
              role="button"
              tabIndex={0}
              aria-pressed={on}
              aria-label={`Show the kernels of ${s.label}`}
              className="cursor-pointer outline-none"
              onMouseEnter={() => setActive(s.id)}
              onFocus={() => setActive(s.id)}
              onClick={() => setActive(s.id)}
            >
              <rect
                x={s.x}
                y={LANE.spans - 11}
                width={s.w}
                height="22"
                fill="var(--color-ws-span)"
                opacity={on ? 1 : 0.45}
              />
              <rect x={s.x} y={LANE.spans + 11} width="2" height={LANE.launch - LANE.spans - 11} fill="var(--color-ws-span)" opacity={on ? 0.7 : 0.25} />
              <text
                x={s.x + 7}
                y={LANE.spans + 4}
                fontSize="11.5"
                className="font-mono"
                fill={on ? '#1b1410' : 'currentColor'}
              >
                {s.label}
              </text>
            </g>
          );
        })}

        <g className="font-mono" fontSize="10.5">
          <text x="40" y={LANE.gpu + 34} className="fill-fd-muted-foreground">
            kernels run after the CPU moved on, and keep their span
          </text>
        </g>
      </svg>
      <figcaption className="font-ui mt-3 flex flex-wrap items-baseline gap-x-3 gap-y-1 text-xs text-fd-muted-foreground">
        <span className="text-fd-foreground">{active ?? 'hover a span'}</span>
        <span>{shown.map(k => k.name).join(' · ')}</span>
      </figcaption>
    </figure>
  );
}
