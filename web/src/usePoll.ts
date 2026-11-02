import { useEffect, useRef } from "react";

/**
 * Runs `fn` now and then every `ms` milliseconds, starting each wait only
 * once the previous call has finished. Unlike setInterval, a slow backend
 * then gets one request at a time instead of a pile of them, which would
 * use up the browser's few connections to the server and stall every page.
 * It starts over when `deps` change; `fn` may change freely.
 */
export function usePoll(fn: () => Promise<unknown> | unknown, ms: number, deps: unknown[] = []) {
  const latest = useRef(fn);
  latest.current = fn;
  useEffect(() => {
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const tick = async () => {
      try {
        await latest.current();
      } catch {
        // Each caller shows its own errors; keep polling.
      }
      if (!cancelled) timer = setTimeout(tick, ms);
    };
    tick();
    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [ms, ...deps]);
}
