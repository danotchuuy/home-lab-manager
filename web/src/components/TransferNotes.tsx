import type { TransferPreview } from "../types";

/** What an import or clone brings along, and what to look out for. */
export function TransferNotes({ preview }: { preview: TransferPreview }) {
  return (
    <>
      {preview.warnings.length > 0 && (
        <ul className="warnings transfer-warnings">
          {preview.warnings.map((w, i) => (
            <li key={i}>⚠ {w}</li>
          ))}
        </ul>
      )}
      <p className="muted small">
        {preview.settings.length > 0
          ? `Also comes along: ${preview.settings.join(", ")}.`
          : "It has no scheduled tasks, backup plan or other settings to bring along."}
      </p>
    </>
  );
}
