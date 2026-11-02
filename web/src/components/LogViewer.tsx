import { memo, useEffect, useRef, useState } from "react";
import { api } from "../api";
import { ansiCSS, parseAnsiLine, type AnsiSegment, type AnsiStyle } from "../ansi";

const MAX_LINES = 5000;

/** A line of output, split into ANSI-styled segments. */
interface LogLine {
  id: number;
  segments: AnsiSegment[];
}

/** One line; memoized so new output doesn't re-render the lines above it. */
const Line = memo(function Line({ segments }: { segments: AnsiSegment[] }) {
  return (
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
});

/** Follows a server's logs over server-sent events; `url` follows another container's instead. */
export function LogViewer({ serverId, url, running }: { serverId?: string; url?: string; running: boolean }) {
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
    const es = new EventSource(src);
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
    es.onopen = () => setConnected(true);
    es.onmessage = (e) => {
      pending.push(e.data);
      if (!frame) frame = requestAnimationFrame(flush);
    };
    // The backend sends "end" when the container stops; don't let the
    // browser reconnect and replay the tail.
    es.addEventListener("end", () => {
      setConnected(false);
      es.close();
    });
    es.onerror = () => setConnected(false);
    return () => {
      es.close();
      if (frame) cancelAnimationFrame(frame);
    };
    // Reconnect when the container starts again.
  }, [src, running]);

  useEffect(() => {
    const box = boxRef.current;
    if (follow && box) box.scrollTop = box.scrollHeight;
  }, [lines, follow]);

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
          lines.map((l) => <Line key={l.id} segments={l.segments} />)
        )}
      </pre>
    </div>
  );
}
