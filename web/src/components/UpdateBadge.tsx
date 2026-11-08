import type { AutoUpdateState, UpdateStatus } from "../types";
import { timeAgo, timeUntil } from "../util";

export function UpdateBadge({ update: u, auto, state }: { update?: UpdateStatus; auto?: boolean; state?: AutoUpdateState }) {
  if (!u) return null;
  const checked = `Checked ${timeAgo(u.checkedAt)}`;
  switch (u.state) {
    case "available": {
      // Updates applied automatically say when.
      const how = !auto
        ? ""
        : state?.state === "waiting" && state.next
          ? ` · auto ${timeUntil(state.next)}`
          : state?.state === "failed"
            ? " · auto-update failed"
            : state?.state === "players"
              ? " · auto once players leave"
            : state
              ? ` · auto: ${state.state}`
              : " · updating automatically";
      return (
        <span className="badge update-available" title={`${u.reason ?? ""}\n${checked}${state?.message ? `\n${state.message}` : ""}`}>
          Update available{how}
        </span>
      );
    }
    case "current":
      return (
        <span className="badge update-current" title={checked}>
          Up to date
        </span>
      );
    case "error":
      return (
        <span className="badge update-error" title={`${u.reason ?? ""}\n${checked}`}>
          Update check failed
        </span>
      );
    default:
      return (
        <span className="badge" title={`${u.reason ?? ""}\n${checked}`}>
          Update status unknown
        </span>
      );
  }
}
