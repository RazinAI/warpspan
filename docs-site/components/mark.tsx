// The Warpspan mark: a span (the bar on top) and the kernels it launched
// (the blocks below), joined where they line up.
export function WarpspanMark({
  className,
  title = 'Warpspan',
}: {
  className?: string;
  title?: string;
}) {
  return (
    <svg viewBox="0 0 64 64" className={className} role="img" aria-label={title}>
      <rect x="6" y="10" width="52" height="12" fill="var(--color-ws-span)" />
      <rect x="6" y="22" width="3" height="20" fill="var(--color-ws-span)" opacity="0.55" />
      <rect x="6" y="38" width="13" height="16" fill="var(--color-ws-kernel)" />
      <rect x="23" y="38" width="21" height="16" fill="var(--color-ws-kernel)" />
      <rect x="48" y="38" width="10" height="16" fill="var(--color-ws-kernel)" />
    </svg>
  );
}
