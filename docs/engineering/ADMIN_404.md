# 404 in the admin panel: `$1` in `proxy_pass` drifted

An analysis of the complaint "the panel shows `Endpoint not found (404). The
server is likely running an older build — deploy the latest and restart it.`,
we rebuilt and restarted the server and it did not help".

---

## The short answer

The messenger had nothing to do with it, and there was nothing to rebuild.

nginx on the admin host was assembling the **wrong upstream path**:
substituting `$1` into `proxy_pass` yielded data from the start of the URI
instead of from the capture group. From there the chain unwound by itself:

```
browser   GET /admin-api/metrics
nginx     $1 = "/admin-"  instead of "metrics"
          → /admin//admin- went upstream
Go        normalises the double slash → 301 Location: /admin/admin-
browser   fetch silently follows the redirect to /admin/admin-
nginx     that is the panel's static content, no such file → 404
panel     "Endpoint not found (404)… older build"
```

The panel's hint pointed at the application host — that is, at **the wrong
machine**.

---

## The fingerprint of the fault

Taken from the production host (`error.log` plus `curl -D -`):

| request | `$1` should be | what was substituted | which is | sent upstream | Go's answer |
|---|---|---|---|---|---|
| `/admin-api/metrics` | `metrics` (7) | `/admin-` (7) | the first **7** characters of the URI | `/admin//admin-` | 301 → `/admin/admin-` |
| `/admin-api/users?…` | `users` (5) | `/admi` (5) | the first **5** characters of the URI | `/admin//admi` | 301 → `/admin/admi` |

The length is right, the base is not: the offset became "the start of the URI"
instead of "the start of the capture group". It matched on every panel request
without exception.

That the 301 came from Go is visible in the response body (`<a href="…">Moved
Permanently</a>.` — the `http.Redirect` format, with `&` in the query escaped
as `&amp;`, which makes `content-length` add up to the byte) and in its CSP
header on the response.

---

## Why this was so hard to catch

- `nginx -t` says nothing: the configuration is syntactically flawless.
- `access.log` shows a `301` rather than an error — it looks like normal
  behaviour.
- `error.log` complains about **static content** (`open() ".../admin/admin-"
  failed`), so the panel's directory is blamed rather than the proxy.
- The backend looks guilty, because the 301 genuinely does come from it.
- Locally on nginx 1.24 (HTTP/1.1 and HTTP/2 alike) it **does not reproduce**:
  the same configuration behaves correctly. The fault depends on what else runs
  in the request before `proxy_pass`.

`$1` in `proxy_pass` is evaluated **lazily**, after the access phase. By that
point the numbered capture groups may point elsewhere. This is a property of
the construct rather than a one-off glitch — which is why it is cured by
replacement, not by a restart.

---

## The fix

The first attempt — moving the tail with a `rewrite` instead of substituting
into `proxy_pass` — **did not help: `$1` drifts in the rewrite phase too.** On
the production host, `rewrite ^/admin-api/(.*)$ /admin/$1 break;` produced
exactly the same `/admin//admin-`. So the cause is not lazy evaluation inside
`proxy_pass` but the capture groups themselves on that machine.

Capture groups were therefore removed everywhere. The upstream path is **not
rewritten at all** — there is nothing to rewrite, so there is nothing to break:

```nginx
# admin host: proxy_pass with no URI and with a variable sends $uri as it is
location /admin-api/ {
    set $backend "chat.example.com";
    proxy_pass https://$backend;
}
```

```go
// the messenger accepts the foreign prefix alongside its own
http.HandleFunc("/admin-api/", adminAPIAliasHandler)   // → /admin/*
```

The variable in `proxy_pass` is deliberate: it forces nginx to re-resolve DNS,
and the backend name is dynamic. The variable-free version —
`proxy_pass https://chat.example.com/admin/;` — also avoids capture groups
(nginx substitutes the prefix itself, natively) and works as a quick patch, but
it resolves the name once at start-up: after an address change at the provider
the panel will keep hitting the wrong machine until `systemctl reload nginx`.

Verified by running the chain: the path, the query string and percent encoding
(`user=a%20b`) arrive intact, and an unknown endpoint returns a marked 404.

### The line-order trap (relevant if a rewrite is reintroduced)

`set` must come **before** `rewrite`. `break` ends the rewrite phase, and `set`
is a directive of that same phase: placed below, it will not run at all.
`$backend` stays empty, `proxy_pass` assembles into a bare `https://`, and
nginx answers **500** with this line in `error.log`:

```
invalid URL prefix in "https://"
```

`nginx -t` is silent about it — the configuration is syntactically correct and
the error arises on a request. Verified by measurement: the same three lines
with `set` and `rewrite` swapped give a 500 on every request.

## Related changes

**The server stopped answering 200 for a non-existent endpoint.** The catch-all
`/` returned the messenger's `index.html` with a 200, so an unknown `/admin/*`
never produced a 404. The `/admin/` subtree now carries `adminUnknownHandler`:
a 404 with the marker `unknown admin endpoint`, behind `adminGuard` — an
enumeration of endpoints must not be handed to someone who has not presented a
key. Exact routes are not intercepted by the subtree (pinned by a negative
control in the test).

**The panel names the cause instead of guessing.** Three different 404s are now
distinguishable:

| What the panel sees | Where to fix it |
|---|---|
| a 404 after a **redirect** (`r.redirected`) | nginx on the admin host: the proxy assembled the wrong path |
| a 404 **with the marker** `unknown admin endpoint` | the messenger: the build is old and the endpoint really is absent |
| a 404 with **neither** | nginx on the admin host: there is no `/admin-api/` route at all |

The contract between `server/admin.go` and `admin-index.html` is pinned by the
test `TestPanelMatchesServerMarker`: the files live in different directories and
are edited separately.

**`location / { return 404; }` gained `no-store`** — `return 404` has no cache
headers of its own, and such a response would outlive the fix to the config.

---

## A separate finding: a hole in `ADMIN_ALLOWED_IPS`

In the deployed admin-host configuration (this was never in the repository) two
lines had appeared inside `location /admin-api/`:

```nginx
proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
proxy_set_header X-Real-IP $remote_addr;
```

Further down in the same block those headers are deliberately blanked. **The
blanking does not take effect:** two `proxy_set_header` directives with the same
name in one block do not override each other — the first one wins. Measured on
a reproduction, what reached the server was:

```
X-Forwarded-For: 9.9.9.9, <admin host>, <local nginx>
```

where `9.9.9.9` was sent by the **browser**. `getIP` takes the first element,
so `ADMIN_ALLOWED_IPS` is compared against a string under the control of
whoever sent the request. Legitimate access breaks at the same time: the
operator's browser address is what gets compared, while the list holds the
address of the host.

Those two lines must be **removed** from the production host. In
`docs/admin-panel/nginx-admin-panel.conf` a warning stands in their place —
they have been added there twice already, because the blanking further down
looks like an oversight.
