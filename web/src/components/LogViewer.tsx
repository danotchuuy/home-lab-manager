import { memo, useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { api, followEvents } from "../api";
import { ansiCSS, parseAnsiLine, type AnsiSegment, type AnsiStyle } from "../ansi";

const MAX_LINES = 5000;

/** What a line is: output from the container, or a console command, its reply or an error added here. */
export type LineKind = "command" | "reply" | "error";

/** A line of output, split into ANSI-styled segments. */
interface LogLine {
  id: number;
  segments: AnsiSegment[];
  kind?: LineKind;
}

/** Adds lines of the given kind to the console, below the output so far. */
export type AppendLines = (text: string, kind: LineKind) => void;

/** One line; memoized so new output doesn't re-render the lines above it. */
const Line = memo(function Line({ segments, kind }: { segments: AnsiSegment[]; kind?: LineKind }) {
  const content = (
    <>
      {segments.map((s, i) => {
        const css = ansiCSS(s.style);
        return css ? (
          <span key={i} style={css}>
            {s.text}
          </span>
        ) : (
          s.text
        );
      })}
      {"\n"}
    </>
  );
  return kind ? <span className={`console-${kind}`}>{content}</span> : content;
});

/**
 * Follows a server's logs as server-sent events; `url` follows another container's instead. `footer` is shown
 * under the output, and can add lines to it, such as console commands and their replies.
 */
export function LogViewer({
  serverId,
  url,
  running,
  footer,
}: {
  serverId?: string;
  url?: string;
  running: boolean;
  footer?: (append: AppendLines) => ReactNode;
}) {
  const src = url ?? api.logsUrl(serverId ?? "");
  const [lines, setLines] = useState<LogLine[]>([]);
  // Colors carry over from one line to the next, as in a terminal.
  const style = useRef<AnsiStyle>({});
  const nextId = useRef(0);
  const [connected, setConnected] = useState(false);
  const [follow, setFollow] = useState(true);
  const boxRef = useRef<HTMLPreElement>(null);

  useEffect(() => {
    setLines([]);
    style.current = {};
    const stop = new AbortController();
    // Batch incoming lines into one state update per animation frame.
    let pending: string[] = [];
    let frame = 0;
    const flush = () => {
      frame = 0;
      const batch = pending.map((text) => {
        const { segments, end } = parseAnsiLine(text, style.current);
        style.current = end;
        return { id: nextId.current++, segments };
      });
      pending = [];
      setLines((prev) => {
        const next = prev.concat(batch);
        return next.length > MAX_LINES ? next.slice(next.length - MAX_LINES) : next;
      });
    };
    const onMessage = (data: string) => {
      pending.push(data);
      if (!frame) frame = requestAnimationFrame(flush);
    };
    (async () => {
      // Reconnect after a dropped connection, as EventSource would, but not
      // after "end", which the backend sends when the container stops:
      // reconnecting would replay the tail.
      for (let first = true; !stop.signal.aborted; first = false) {
        if (!first) await new Promise((r) => setTimeout(r, 3000));
        if (stop.signal.aborted) return;
        try {
          if (!first) {
            setLines([]);
            style.current = {};
          }
          const { ended } = await followEvents(src, { open: () => setConnected(true), message: onMessage }, stop.signal);
          setConnected(false);
          if (ended) return;
        } catch {
          setConnected(false);
        }
      }
    })();
    return () => {
      stop.abort();
      if (frame) cancelAnimationFrame(frame);
    };
    // Reconnect when the container starts again.
  }, [src, running]);

  useEffect(() => {
    const box = boxRef.current;
    if (follow && box) box.scrollTop = box.scrollHeight;
  }, [lines, follow]);

  const append = useCallback<AppendLines>((text, kind) => {
    const added = text.split("\n").map((t) => ({ id: nextId.current++, segments: parseAnsiLine(t, {}).segments, kind }));
    setLines((prev) => {
      const next = prev.concat(added);
      return next.length > MAX_LINES ? next.slice(next.length - MAX_LINES) : next;
    });
    // A command is worth seeing the answer to.
    if (kind === "command") setFollow(true);
  }, []);

  function onScroll() {
    const box = boxRef.current;
    if (!box) return;
    setFollow(box.scrollHeight - box.scrollTop - box.clientHeight < 40);
  }

  return (
    <div className="log-viewer">
      <div className="log-toolbar">
        <span className={`dot ${connected ? "on" : "off"}`} />
        <span>{connected ? "Live" : "Not following"}</span>
        <div className="spacer" />
        <button className="btn ghost small" onClick={() => setLines([])}>
          Clear
        </button>
        {!follow && (
          <button className="btn ghost small" onClick={() => setFollow(true)}>
            Jump to bottom
          </button>
        )}
      </div>
      <pre ref={boxRef} className="log-box" onScroll={onScroll}>
        {lines.length === 0 ? (
          <span className="muted">No log output yet.</span>
        ) : (
          lines.map((l) => <Line key={l.id} segments={l.segments} kind={l.kind} />)
        )}
      </pre>
      {footer?.(append)}
    </div>
  );
}
