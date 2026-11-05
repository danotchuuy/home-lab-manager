// Passkeys in the browser: hlm sends WebAuthn options with binary values as
// base64url, and takes the browser's answers the same way. The conversion is
// done here rather than with PublicKeyCredential.parseCreationOptionsFromJSON,
// which older browsers lack.

function fromB64(s: string): ArrayBuffer {
  const b = atob(
    s
      .replace(/-/g, "+")
      .replace(/_/g, "/")
      .padEnd(Math.ceil(s.length / 4) * 4, "="),
  );
  const out = new Uint8Array(b.length);
  for (let i = 0; i < b.length; i++) out[i] = b.charCodeAt(i);
  return out.buffer;
}

function toB64(buf: ArrayBuffer | null | undefined): string {
  if (!buf) return "";
  const bytes = new Uint8Array(buf);
  let s = "";
  for (const b of bytes) s += String.fromCharCode(b);
  return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

interface Descriptor {
  type: "public-key";
  id: string;
  transports?: string[];
}

/** navigator.credentials.create() options, as hlm sends them. */
export interface CreationOptionsJSON {
  publicKey: {
    rp: { id: string; name: string };
    user: { id: string; name: string; displayName: string };
    challenge: string;
    pubKeyCredParams: { type: "public-key"; alg: number }[];
    timeout: number;
    attestation: AttestationConveyancePreference;
    authenticatorSelection: AuthenticatorSelectionCriteria;
    excludeCredentials: Descriptor[];
  };
}

/** navigator.credentials.get() options, as hlm sends them. */
export interface RequestOptionsJSON {
  publicKey: {
    challenge: string;
    rpId: string;
    timeout: number;
    userVerification: UserVerificationRequirement;
    allowCredentials: Descriptor[];
  };
}

export interface NewPasskeyJSON {
  clientDataJSON: string;
  attestationObject: string;
  transports: string[];
}

export interface UsedPasskeyJSON {
  id: string;
  clientDataJSON: string;
  authenticatorData: string;
  signature: string;
  userHandle: string;
}

const descriptor = (d: Descriptor): PublicKeyCredentialDescriptor => ({
  type: d.type,
  id: fromB64(d.id),
  transports: d.transports as AuthenticatorTransport[] | undefined,
});

/**
 * Why passkeys can't be used on this page, or undefined if they can: they
 * need a secure context (HTTPS, or localhost) and a browser that has them.
 */
export function passkeysUnavailable(): string | undefined {
  if (!window.isSecureContext) {
    return "Passkeys need hlm opened over HTTPS (or as localhost); this page is plain HTTP.";
  }
  if (!window.PublicKeyCredential || !navigator.credentials) return "This browser doesn't support passkeys.";
  if (/^\d+\.\d+\.\d+\.\d+$|^\[/.test(window.location.hostname)) {
    return "Passkeys need hlm opened by a name, not an IP address.";
  }
  return undefined;
}

/** Makes a passkey with the options hlm sent. */
export async function createPasskey(o: CreationOptionsJSON): Promise<NewPasskeyJSON> {
  const p = o.publicKey;
  const cred = (await navigator.credentials.create({
    publicKey: {
      rp: p.rp,
      user: { id: fromB64(p.user.id), name: p.user.name, displayName: p.user.displayName },
      challenge: fromB64(p.challenge),
      pubKeyCredParams: p.pubKeyCredParams,
      timeout: p.timeout,
      attestation: p.attestation,
      authenticatorSelection: p.authenticatorSelection,
      excludeCredentials: (p.excludeCredentials ?? []).map(descriptor),
    },
  })) as PublicKeyCredential | null;
  if (!cred) throw new Error("No passkey was made.");
  const r = cred.response as AuthenticatorAttestationResponse;
  return {
    clientDataJSON: toB64(r.clientDataJSON),
    attestationObject: toB64(r.attestationObject),
    transports: typeof r.getTransports === "function" ? r.getTransports() : [],
  };
}

/** Uses a passkey with the options hlm sent. */
export async function getPasskey(o: RequestOptionsJSON): Promise<UsedPasskeyJSON> {
  const p = o.publicKey;
  const cred = (await navigator.credentials.get({
    publicKey: {
      challenge: fromB64(p.challenge),
      rpId: p.rpId,
      timeout: p.timeout,
      userVerification: p.userVerification,
      allowCredentials: (p.allowCredentials ?? []).map(descriptor),
    },
  })) as PublicKeyCredential | null;
  if (!cred) throw new Error("No passkey was used.");
  const r = cred.response as AuthenticatorAssertionResponse;
  return {
    id: toB64(cred.rawId),
    clientDataJSON: toB64(r.clientDataJSON),
    authenticatorData: toB64(r.authenticatorData),
    signature: toB64(r.signature),
    userHandle: toB64(r.userHandle),
  };
}

/** A browser's passkey error, in words: cancelling isn't an error to dwell on. */
export function passkeyError(e: unknown): string {
  if (e instanceof DOMException) {
    if (e.name === "NotAllowedError") return "The passkey wasn't used (cancelled, or it timed out).";
    if (e.name === "InvalidStateError") return "That passkey is already added to this account.";
    if (e.name === "SecurityError") return "This page's address can't use passkeys: open hlm over HTTPS by its name.";
  }
  return (e as Error).message;
}
