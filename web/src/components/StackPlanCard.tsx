import type { StackPlan } from "../types";

const CHANGE: Record<string, string> = {
  create: "created",
  update: "recreated",
  unchanged: "unchanged",
  remove: "removed (its data is kept)",
  adopt: "recreated as hlm's (same name, image and data)",
};

/** What applying (or adopting) a Compose stack would do, service by service. */
export function StackPlanCard({ plan }: { plan: StackPlan }) {
  return (
    <div className="card">
      <h2>What happens</h2>
      {plan.warnings.length > 0 && (
        <ul className="warnings transfer-warnings">
          {plan.warnings.map((w, i) => (
            <li key={i}>⚠ {w}</li>
          ))}
        </ul>
      )}
      <table className="data-table">
        <thead>
          <tr>
            <th>Service</th>
            <th>Image</th>
            <th>Ports</th>
            <th>Data</th>
            <th>Change</th>
          </tr>
        </thead>
        <tbody>
          {plan.services.map((s) => (
            <tr key={s.service}>
              <td>
                <strong>{s.service}</strong>
                <div className="muted small">{s.container}</div>
                {s.dependsOn?.length ? <div className="muted small">after {s.dependsOn.join(", ")}</div> : null}
              </td>
              <td>
                <code className="small">{s.image}</code>
              </td>
              <td className="small">{s.ports.join(", ") || "—"}</td>
              <td className="small">
                {s.mounts.map((m) => (
                  <div key={m}>
                    <code>{m}</code>
                  </div>
                ))}
              </td>
              <td className={`small change-${s.change}`}>{CHANGE[s.change]}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {plan.networks.length > 0 && <p className="muted small">Networks: {plan.networks.join(", ")}</p>}
    </div>
  );
}
