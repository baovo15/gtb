// TerminalView shows a peek of the item's worker terminal. It refreshes on
// demand and every timing.terminalMs while the page is visible; requests
// never overlap (the server may wait for the machine).
import { useCallback, useEffect, useRef, useState } from "react";
import { api, errorText, itemPath, timing } from "../api";

export function TerminalView({ machine, itemId }: { machine: string; itemId: string }) {
  const [lines, setLines] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const inflight = useRef(false);
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const alive = useRef(true);

  const refresh = useCallback(async () => {
    if (inflight.current) return;
    inflight.current = true;
    setBusy(true);
    clearTimeout(timer.current);
    try {
      const res = await api<{ lines: string }>(`${itemPath(machine, itemId)}/terminal`);
      if (!alive.current) return;
      setLines(res.lines);
      setError(null);
    } catch (e) {
      if (!alive.current) return;
      setError(errorText(e));
    } finally {
      inflight.current = false;
      if (alive.current) {
        setBusy(false);
        if (document.visibilityState === "visible") {
          timer.current = setTimeout(() => void refresh(), timing.terminalMs);
        }
      }
    }
  }, [machine, itemId]);

  useEffect(() => {
    alive.current = true;
    void refresh();
    const onVis = () => {
      if (document.visibilityState === "visible") void refresh();
      else clearTimeout(timer.current);
    };
    document.addEventListener("visibilitychange", onVis);
    return () => {
      alive.current = false;
      clearTimeout(timer.current);
      document.removeEventListener("visibilitychange", onVis);
    };
  }, [refresh]);

  return (
    <div className="flex flex-col gap-2">
      <div className="flex items-center gap-3">
        <button
          type="button"
          aria-label="Refresh terminal"
          onClick={() => void refresh()}
          disabled={busy}
          className="rounded-md border border-zinc-300 px-3 py-1 text-sm hover:bg-zinc-100 focus-visible:outline-2 focus-visible:outline-teal-500 disabled:opacity-50 dark:border-zinc-700 dark:hover:bg-zinc-800"
        >
          {busy ? "Refreshing…" : "Refresh"}
        </button>
        <span className="text-xs text-zinc-500">Auto-refreshes every {Math.round(timing.terminalMs / 1000)} s while visible.</span>
      </div>
      {error && (
        <p role="alert" className="text-sm text-red-700 dark:text-red-400">
          Terminal unavailable: {error}
        </p>
      )}
      <pre className="max-h-[70vh] min-h-40 overflow-auto rounded-md bg-zinc-950 p-3 font-mono text-xs leading-relaxed text-zinc-100" aria-label="Terminal output">
        {lines ?? (error ? "" : "Loading…")}
      </pre>
    </div>
  );
}
