// Freshness is how long ago the item's machine last reported. A stale
// machine (beyond the server's threshold) shows a grey "stale" badge.
export function freshnessLabel(seconds: number): string {
  const s = Math.max(0, Math.floor(seconds));
  if (s < 60) return `${s}s ago`;
  return `${Math.floor(s / 60)}m ago`;
}

export function Freshness({ seconds, stale }: { seconds: number; stale: boolean }) {
  if (stale) {
    return (
      <span
        className="rounded-full bg-zinc-200 px-1.5 py-0.5 font-mono text-[11px] text-zinc-600 dark:bg-zinc-800 dark:text-zinc-400"
        title={`machine last seen ${freshnessLabel(seconds)}`}
      >
        stale
      </span>
    );
  }
  return (
    <span className="rounded-full bg-emerald-50 px-1.5 py-0.5 font-mono text-[11px] text-emerald-700 dark:bg-emerald-950 dark:text-emerald-300" title="machine last seen">
      {freshnessLabel(seconds)}
    </span>
  );
}
