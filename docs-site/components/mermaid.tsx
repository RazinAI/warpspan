'use client';
import { useTheme } from 'next-themes';
import { useEffect, useId, useRef, useState } from 'react';

// Warpspan's palette (app/global.css), as mermaid needs real colours to derive
// its shades from. "route" is the ember of a kernel, "accent" a saffron tint.
const palettes = {
  light: {
    background: '#f6f2e8',
    text: '#1d1915',
    muted: '#5d524a',
    card: '#fbf8f1',
    border: '#d6ccbd',
    route: '#b8571f',
    accent: '#f1e5cf',
    accentText: '#6b3f14',
    group: '#ece4d5',
  },
  dark: {
    background: '#16130f',
    text: '#f3eee4',
    muted: '#b3a796',
    card: '#1c1813',
    border: '#3d3631',
    route: '#e9873a',
    accent: '#2e2419',
    accentText: '#f2c98a',
    group: '#211d18',
  },
};

function themeVariables(dark: boolean, fontFamily: string) {
  const c = dark ? palettes.dark : palettes.light;
  return {
    darkMode: dark,
    fontFamily,
    fontSize: '14px',
    background: c.background,
    textColor: c.text,
    lineColor: c.muted,
    primaryColor: c.card,
    primaryTextColor: c.text,
    primaryBorderColor: c.route,
    secondaryColor: c.accent,
    tertiaryColor: c.group,
    mainBkg: c.card,
    nodeBorder: c.route,
    clusterBkg: c.group,
    clusterBorder: c.border,
    titleColor: c.text,
    edgeLabelBackground: c.background,
    actorBkg: c.card,
    actorBorder: c.route,
    actorTextColor: c.text,
    actorLineColor: c.border,
    signalColor: c.text,
    signalTextColor: c.text,
    labelBoxBkgColor: c.card,
    labelBoxBorderColor: c.border,
    labelTextColor: c.text,
    loopTextColor: c.muted,
    noteBkgColor: c.accent,
    noteTextColor: c.accentText,
    noteBorderColor: c.border,
    activationBkgColor: c.accent,
    activationBorderColor: c.route,
    sequenceNumberColor: c.background,
  };
}

// Renders a ```mermaid block (turned into <Mermaid chart="…" /> by
// remarkMdxMermaid) in the browser, redrawn when the theme changes.
export function Mermaid({ chart }: { chart: string }) {
  const id = useId().replace(/[^a-zA-Z0-9]/g, '');
  const { resolvedTheme } = useTheme();
  const [svg, setSvg] = useState('');
  const [failed, setFailed] = useState(false);
  const figure = useRef<HTMLElement>(null);

  useEffect(() => {
    if (!resolvedTheme) return;
    let cancelled = false;
    (async () => {
      const { default: mermaid } = await import('mermaid');
      mermaid.initialize({
        startOnLoad: false,
        securityLevel: 'strict',
        theme: 'base',
        themeVariables: themeVariables(
          resolvedTheme === 'dark',
          getComputedStyle(document.body).fontFamily,
        ),
        flowchart: { curve: 'basis', padding: 12 },
        sequence: { mirrorActors: false, messageMargin: 32 },
      });
      const { svg } = await mermaid.render(`mermaid-${id}`, chart);
      if (!cancelled) setSvg(svg);
    })().catch(error => {
      console.error('mermaid', error);
      if (!cancelled) setFailed(true);
    });
    return () => {
      cancelled = true;
    };
  }, [chart, id, resolvedTheme]);

  // Shrinks a wide diagram to fit, but not past three quarters of its size:
  // on a phone it scrolls sideways instead of turning its text unreadable.
  useEffect(() => {
    const element = figure.current?.querySelector('svg');
    const width = element?.viewBox.baseVal?.width;
    if (!element || !width) return;
    element.style.width = `${width}px`;
    element.style.maxWidth = '100%';
    element.style.minWidth = `${Math.round(width * 0.75)}px`;
  }, [svg]);

  if (failed)
    return (
      <pre className="my-6 overflow-x-auto rounded-xl border bg-fd-card p-4 text-sm">
        {chart}
      </pre>
    );

  return (
    <figure
      ref={figure}
      className="not-prose my-6 flex min-h-32 overflow-x-auto rounded-xl border bg-fd-card p-4 [&_svg]:mx-auto [&_svg]:h-auto [&_svg]:shrink-0"
      aria-label="Diagram"
      dangerouslySetInnerHTML={{ __html: svg }}
    />
  );
}
