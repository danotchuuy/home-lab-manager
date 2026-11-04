# 12 — Two-factor sign-in (TOTP and passkeys)

**Priority:** Medium · **Depends on:** nothing

## Why

hlm controls the Docker socket, which is root on the host. Local accounts are protected by a password (argon2id, with
lockout backoff). SSO users get MFA from their provider, but local accounts — the recommended fallback — don't.

## What it does

### TOTP (authenticator app) — v1

- **My account → Two-factor**: **Set up** shows a QR code (`otpauth://totp/hlm:alice?secret=…&issuer=hlm`) and the
  secret as text; the user enters a code to confirm. Shows **10 recovery codes** once.
- Sign-in becomes password → code (or recovery code). The "confirm it's you" step (`auth/confirm.go`) also asks for a
  code when the account has 2FA.
- Admin setting **Require two-factor for local accounts** (all / admins only / off). Accounts without it are sent to
  setup right after the password.
- Admins can **Reset two-factor** for another account (confirmed). `hlm reset-password` gains `--reset-2fa`.

### Passkeys / WebAuthn — v2

- Register one or more passkeys (platform or security keys) as a second factor, or as a passwordless sign-in.
- Needs HTTPS (or `localhost`) and a stable origin: the HTTPS hostname from `tlscert` — show this requirement in the UI.

## Design

- **TOTP** (RFC 6238): HMAC-SHA1, 6 digits, 30 s step, accept ±1 step. Standard library only (`crypto/hmac`,
  `encoding/base32`). Remember the last used step per account to stop replay.
- Store on `Account` (secrets store): `TOTPSecret` (encrypted with the rest), `RecoveryHashes []string` (argon2id like
  passwords), `TOTPLastStep`.
- **Flow:** `POST /api/auth/login` with correct password and 2FA on → `{"twoFactor": true}` and a short-lived (5 min),
  signed, HttpOnly "half sign-in" cookie; `POST /api/auth/login/2fa {code}` completes it. Wrong codes count toward the
  same lockout backoff as passwords.
- **API keys / SSH-key tokens** are unaffected (they're already a second kind of credential and are scoped by
  "allowed from").
- **QR code**: generate client-side in the UI (small QR encoder lib or a tiny in-repo encoder) so the secret never
  goes to a third party.
- **WebAuthn (v2):** needs CBOR/COSE parsing; consider `github.com/go-webauthn/webauthn` — a deliberate dependency
  choice, discuss first.

## API

| Endpoint | Policy | |
| --- | --- | --- |
| `POST /api/auth/login/2fa` | public (needs half cookie) | |
| `POST /api/auth/accounts/{username}/2fa/setup` | signedIn (self) + confirmed | Returns secret + otpauth URL |
| `POST /api/auth/accounts/{username}/2fa/enable` | signedIn (self) | `{code}` → recovery codes |
| `DELETE /api/auth/accounts/{username}/2fa` | confirmed (self, or admin for others) | |
| `POST /api/auth/accounts/{username}/2fa/recovery` | confirmed (self) | New recovery codes |
| `PUT /api/auth/config` (existing) | confirmed | `requireTwoFactor` |

## Lockout safety

- Requiring 2FA can only be turned on by an admin who has it set up (mirrors "Require sign-in can only be turned on
  while signed in").
- `hlm reset-password --reset-2fa` on the host is the last resort; document it next to the existing "Locked out?".

## Testing

- TOTP against RFC 6238 test vectors; skew window; replay rejected.
- Login flow: password-only accounts unchanged; 2FA account needs code; half cookie expiry; recovery code single use;
  lockout counting.
- Confirm-it's-you with 2FA.

## Tasks

- [ ] TOTP implementation + tests
- [ ] Account fields + recovery codes
- [ ] Two-step login + half-session cookie
- [ ] Confirm step asks for code
- [ ] Require-2FA setting + forced setup
- [ ] Admin reset + CLI flag
- [ ] UI: setup with QR, recovery codes, sign-in code page
- [ ] (v2) Passkeys
- [ ] README section
