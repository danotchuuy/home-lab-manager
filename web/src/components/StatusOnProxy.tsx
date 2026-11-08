import { Link } from "react-router-dom";
import type { ProxySettings, ProxyStatusSites } from "../types";

/**
 * Serving the status page on the nginx proxy server: its sites' domains, the
 * proxy's own address, and their HTTPS. The same settings on the Proxy page
 * and under System → Status page; they're part of the proxy's settings.
 */
export function StatusOnProxyFields({
  form,
  set,
  where,
}: {
  form: ProxySettings;
  set: <K extends keyof ProxySettings>(k: K, v: ProxySettings[K]) => void;
  /** The page it's on, for its links. */
  where: "proxy" | "status";
}) {
  return (
    <>
      <label className="radio">
        <input type="checkbox" checked={!!form.statusPage} onChange={(e) => set("statusPage", e.target.checked)} />
        {where === "proxy" ? (
          <>
            Serve the <Link to="/system#status-page">status page</Link> here too
          </>
        ) : (
          <>Serve the status page on the proxy server ({form.host})</>
        )}
      </label>
      <span className="field-help">
        nginx on the proxy server answers the status page&apos;s sites&apos; domains (each site has a switch for it) and,
        if you like, its own address, and passes only the
        status page to hlm, through the WireGuard tunnel: hlm&apos;s own port isn&apos;t opened to the internet. So the
        WireGuard tunnel has to be the home address.
        {form.statusPage && form.upstreamSource !== "wireguard" && (
          <strong className="form-error">
          {" "}
          Choose the WireGuard tunnel as the home address {where === "proxy" ? "above" : <Link to="/proxy">on the Proxy page</Link>}.
        </strong>
        )}
      </span>
      {form.statusPage && (
        <div className="field">
          <label className="radio">
            <input
              type="checkbox"
              checked={!!form.statusOnAddress}
              onChange={(e) => {
                set("statusOnAddress", e.target.checked);
                if (e.target.checked && !form.statusPort) set("statusPort", 80);
              }}
            />
            Also on the proxy server&apos;s own address ({form.host || "its host"}), where players find the game
            servers
          </label>
          {form.statusOnAddress && (
            <label className="field">
              <span className="field-label">Port</span>
              <input
                type="number"
                min={1}
                max={65535}
                value={form.statusPort ?? 80}
                onChange={(e) => set("statusPort", Number(e.target.value) || 0)}
                style={{ maxWidth: "8rem" }}
              />
              <span className="field-help">
                {form.statusAddressHttps ? (
                  <>
                    <code>
                      https://{form.statusAddressName || "your-domain"}
                      {(form.statusPort ?? 80) === 80 ? "" : `:${form.statusPort}`}/status
                    </code>
                    {(form.statusPort ?? 80) === 80 && <> (on 443; port 80 sends visitors there)</>}
                  </>
                ) : (
                  <>
                    Plain HTTP:{" "}
                    <code>
                      http://{form.host || "proxy"}
                      {(form.statusPort ?? 80) === 80 ? "" : `:${form.statusPort}`}/status
                    </code>
                  </>
                )}
                . On 80 it shares the port with the sites&apos; domains; any other port can&apos;t be one a game
                server is relayed on. hlm opens it in the proxy&apos;s Linode firewall, if it manages one.
              </span>
            </label>
          )}
          {form.statusOnAddress && (
            <label className="radio">
              <input
                type="checkbox"
                checked={!!form.statusAddressHttps}
                onChange={(e) => {
                  set("statusAddressHttps", e.target.checked);
                  const host = form.host ?? "";
                  if (e.target.checked && !form.statusAddressName && /[a-z]/i.test(host) && !host.includes(":"))
                    set("statusAddressName", host);
                }}
              />
              HTTPS, with a Let&apos;s Encrypt certificate hlm gets through Cloudflare and keeps on the proxy server
            </label>
          )}
          {form.statusOnAddress && form.statusAddressHttps && (
            <label className="field">
              <span className="field-label">Domain</span>
              <input
                value={form.statusAddressName ?? ""}
                onChange={(e) => set("statusAddressName", e.target.value)}
                placeholder="play.example.com"
              />
              <span className="field-help">
                The name players use for the game servers, pointing at the proxy server (DNS only, not proxied by
                Cloudflare, which can&apos;t carry game traffic). It has to be on the{" "}
                <Link to="/tunnel">Cloudflare account</Link>: hlm proves it&apos;s yours with a DNS record there
                (the token stays with hlm), installs the certificate and its key on the proxy server, readable by
                root alone, and renews it. The Let&apos;s Encrypt email below is used for it if set.
              </span>
            </label>
          )}
        </div>
      )}
      {form.statusPage && (
        <div className="field status-tls">
          <span className="field-label">HTTPS for the sites&apos; domains</span>
          <label className="radio">
            <input type="radio" name="statusTls" checked={!form.statusTls} onChange={() => set("statusTls", "")} />
            None: HTTP only (you put the domains&apos; DNS at the proxy server)
          </label>
          <label className="radio">
            <input
              type="radio"
              name="statusTls"
              checked={form.statusTls === "letsencrypt"}
              onChange={() => set("statusTls", "letsencrypt")}
            />
            Let&apos;s Encrypt: certbot on the proxy server gets and renews free certificates (you point the
            domains&apos; DNS at the proxy server)
          </label>
          <label className="radio">
            <input
              type="radio"
              name="statusTls"
              checked={form.statusTls === "letsencrypt-cloudflare"}
              onChange={() => set("statusTls", "letsencrypt-cloudflare")}
            />
            Let&apos;s Encrypt, DNS on Cloudflare: hlm points each domain straight at the proxy server (a DNS-only
            record) and gets, installs and renews its certificate there through Cloudflare
          </label>
          {form.statusTls === "letsencrypt-cloudflare" && (
            <span className="field-help">
              For domains on the <Link to="/tunnel">Cloudflare account</Link> (the tunnel itself isn&apos;t needed; its
              token needs DNS: Edit). Browsers trust the certificates, visitors reach the proxy directly, and nothing
              else has to be set up. A domain that already goes through the Cloudflare Tunnel says so on its site:
              move it to the proxy there.
            </span>
          )}
          <label className="radio">
            <input
              type="radio"
              name="statusTls"
              checked={form.statusTls === "cloudflare"}
              onChange={() => set("statusTls", "cloudflare")}
            />
            Cloudflare: hlm points each domain at the proxy server through Cloudflare, and installs a Cloudflare
            Origin CA certificate there
          </label>
          {form.statusTls === "cloudflare" && (
            <span className="field-help">
              Uses the <Link to="/tunnel">Cloudflare account</Link> (the tunnel itself isn&apos;t needed): the
              domains have to be on it, and its token needs <strong>Zone → SSL and Certificates: Edit</strong> as
              well as DNS: Edit. Set the domain&apos;s SSL/TLS mode to <strong>Full (strict)</strong> in the
              Cloudflare dashboard. The proxy&apos;s firewall then lets only Cloudflare reach ports 80 and 443.
            </span>
          )}
          {(form.statusTls === "letsencrypt" ||
            form.statusTls === "letsencrypt-cloudflare" ||
            (form.statusOnAddress && form.statusAddressHttps)) && (
            <label className="field">
              <span className="field-label">Email for Let&apos;s Encrypt (optional)</span>
              <input
                type="email"
                value={form.statusEmail ?? ""}
                onChange={(e) => set("statusEmail", e.target.value)}
                placeholder="you@example.com"
              />
              <span className="field-help">Where Let&apos;s Encrypt sends notices about the certificates.</span>
            </label>
          )}
        </div>
      )}
    </>
  );
}

/** What applying did for the status page's sites. */
export function statusNote(r?: ProxyStatusSites): string {
  if (!r) return "";
  if (r.error) return ` The status page's sites weren't served: ${r.error}`;
  const at =
    (r.address ? ` The status page is at ${r.address}.` : "") +
    (r.addressError ? ` No HTTPS on the proxy's address yet: ${r.addressError}.` : "");
  if (r.domains.length === 0) return at || " There are no status page sites to serve yet.";
  const http = r.domains.filter((d) => !r.https.includes(d));
  let note = `${at} Status page sites: ${r.https.length ? `${r.https.join(", ")} over HTTPS` : ""}${r.https.length && http.length ? "; " : ""}${http.length ? `${http.join(", ")} over HTTP` : ""}.`;
  const failed = Object.entries(r.failed ?? {});
  if (failed.length && r.tls === "cloudflare")
    note += ` Not set up on Cloudflare: ${failed.map(([d, why]) => `${d} (${why})`).join(", ")}.`;
  else if (failed.length)
    note += ` No certificate yet for ${failed.map(([d, why]) => `${d} (${why})`).join(", ")}; is its DNS pointing at the proxy server?`;
  if (r.notes?.length) note += " " + r.notes.join(" ");
  return note;
}
