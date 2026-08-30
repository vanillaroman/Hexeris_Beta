# Corporate single sign-on (OIDC)

Single sign-on over Keycloak, Authentik, Entra ID, Okta or Google Workspace.
Disabled by default and **added as a second way in** — passwords and LDAP keep
working.

---

## Why a customer wants it

The security team gets what no local password can give: one place where
accounts live. An employee leaves, they are disabled in Keycloak, and access to
Hexeris disappears along with everything else — no separate tour of the
systems. This is the first question integrators ask, and the answer is now
concrete.

---

## Configuration

```bash
OIDC_ENABLED=true
OIDC_ISSUER=https://sso.example.com/realms/hexeris
OIDC_CLIENT_ID=hexeris
OIDC_CLIENT_SECRET=<from the provider>
OIDC_REDIRECT_URL=https://chat.example.com/auth/oidc/callback
```

Optional:

| Variable | Default | Meaning |
|---|---|---|
| `OIDC_SCOPES` | `openid email profile` | The scopes requested |
| `OIDC_USERNAME_CLAIM` | `email` | Which claim identifies the employee |
| `OIDC_BUTTON_LABEL` | `Sign in with SSO` | The button caption on the sign-in screen |

`OIDC_REDIRECT_URL` must match **character for character** what the provider
has on record: a mismatch is the most common cause of refusal, and providers
report it obscurely.

A missing setting is named individually: `/auth/oidc/status`, called as an
administrator, says which variable is absent rather than just "it does not
work".

---

## Who gets in

An account is created on first sign-in **if** the email domain is in
`ALLOWED_EMAIL_DOMAINS` or self-registration is open. It is the same list that
Google sign-in uses: a second list would mean a second place where a domain can
be left open by mistake.

Without an allowlist and with registration closed, SSO confirms identity but
creates no new employees — accounts are created by an administrator.

---

## Why it is built this way rather than more simply

**The link is held by `issuer + subject`, not by email.** This is a lesson
already paid for by Google sign-in (see `auth.go`): looking up by name or email
means that a match against SOMEONE ELSE'S local record lets you into it. A
person with the address `grace@partner.org` would sign into the record `grace`
created for a different employee. A provider's subject is immutable and never
reused, whereas an email is rewritten when someone's surname changes — so
changing an email does not spawn a second account, and the same subject from
different providers counts as different people.

**Blocking is checked before a token is issued.** Without this, SSO becomes a
way around a block: the administrator blocks an employee, they sign in through
the provider and receive a fresh token whose `iat` is newer than the revocation
mark, so revocation does not catch it.

**PKCE (S256), even though there is a `client_secret`.** An intercepted code is
useless without the verifier, and it costs thirty lines.

**`nonce` is mandatory.** It binds the token to our request. Without it any
valid token from the same provider will do, including one issued for a
different application in the same tenant.

**Signatures are verified against the provider's JWKS**, algorithms are
restricted to RS256/384/512, and keys shorter than 2048 bits are rejected. An
unfamiliar `kid` forces an immediate re-read of the key set (but no more often
than once a minute): without that, a key rotation at the customer would take
sign-in down for everyone at once and be fixable only by a restart.

**The front end receives a one-time code, not a token in the URL.** URLs end up
in the nginx log, in browser history and in `Referer`; a 30-day token would
settle in all three. The code lives two minutes and works exactly once.

**JWKS is parsed by forty lines of our own code, without a library.** It is
JSON carrying a modulus and an exponent in base64url; in a project sold on
simplicity of operation and auditability, a dependency costs more.

---

## Endpoints

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/auth/oidc/status` | Whether sign-in is configured; the button caption |
| `GET` | `/auth/oidc/start` | Leaving for the provider |
| `GET` | `/auth/oidc/callback` | Returning from the provider |
| `POST` | `/auth/oidc/exchange` | Exchanging the one-time code for a token |

---

## Keycloak for a demonstration

It runs alongside and demands nothing of the customer.

```bash
docker run -d --name keycloak -p 8081:8080 \
  -e KEYCLOAK_ADMIN=admin -e KEYCLOAK_ADMIN_PASSWORD=admin \
  quay.io/keycloak/keycloak:26.0 start-dev
```

Then, in the console at `http://<host>:8081`:

1. **Create realm** → `hexeris`.
2. **Clients → Create client** → Client ID `hexeris`, Client authentication
   **On**, Valid redirect URIs — exactly your `OIDC_REDIRECT_URL`.
3. **Credentials** → copy the client secret into `OIDC_CLIENT_SECRET`.
4. **Users → Add user** → set an email, tick **Email verified**, and set a
   password on the Credentials tab.

`OIDC_ISSUER` for this setup: `http://<host>:8081/realms/hexeris`.

To check the configuration before clicking any button:

```bash
curl -s https://<domain>/auth/oidc/status
```

---

## Tests

The server-side tests run under an ordinary `go test` and need no provider
network: they raise their own RSA key and sign their own tokens.

Everything that should be rejected is rejected, and each case is a way of
signing in under someone else's name if the check were skipped: a foreign
`issuer`, a foreign `audience`, an expired token, a token with no `exp`, a
foreign and a missing `nonce`, a signature by a foreign key, `alg=none`.
Separately: an encryption key in place of a signing key, a 1024-bit key, an
empty key set.

Account creation is tested against a live database (`TEST_DATABASE_URL`): a
matching name does not let anyone into someone else's record, a repeat sign-in
returns the same record, changing an email does not spawn a second one, the
same subject from different providers means different people, and closed
registration without an allowlist creates nothing.

The interface is covered by `tests/ui/uitest_sso.js`: the button appears only
when SSO is configured, the refusal reason arrives verbatim, the one-time code
goes off to be exchanged, and the address bar is cleared — neither code nor
token in history.

---

## Deliberately out of scope

- **Single logout** (`end_session_endpoint`): signing out of Hexeris does not
  end the session at the provider. Deliberate — the opposite behaviour is
  surprising when several applications share one provider.
- **Roles and groups from the provider.** Permissions in Hexeris are assigned
  by an administrator; mapping provider groups onto roles is a separate task
  with a model of its own.
- **Token refresh.** A Hexeris session has its own lifetime; after it the
  person signs in again, which with an active provider session is one click.
