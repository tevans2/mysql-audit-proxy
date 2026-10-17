// Whether the proxy is running the config that was saved. 
// Kept pure and separate so the Servers banner and the header pill agree

//heartbeat interval
export const REPORT_STALE_MS = 60 * 1000;

// Returns null when the API has not answered yet, so callers can leave the
// pill hidden rather than claim a state they do not know.
export function configState({ revision, appliedRevision, appliedAt, now = Date.now() }) {
  if (revision === undefined || revision === null) return null;

  const applied = appliedRevision || 0;
  const reportedAt = appliedAt ? Date.parse(appliedAt) : NaN;
  const reported = !Number.isNaN(reportedAt);

  // Nothing has ever been reported: a proxy that has not started, or one that
  // cannot reach the API. Either way we cannot claim the config is live.
  if (!applied && !reported) {
    return { state: "unknown", text: "Saved rev " + revision + ", proxy has not reported" };
  }

  // Saved ahead of applied: the proxy has the change but has not finished it.
  if (applied < revision) {
    return { state: "pending", text: "Saved rev " + revision + ", proxy on rev " + applied };
  }

  // Revisions match, but the last report is old, so the proxy may be gone and
  // the matching number is only the last thing it told us.
  if (!reported || now - reportedAt > REPORT_STALE_MS) {
    return { state: "stale", text: "Proxy last reported rev " + applied };
  }

  return { state: "live", text: "Proxy running rev " + applied };
}