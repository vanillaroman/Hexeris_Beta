# Hexeris — Source-Available Licence

**Version 1.0 — effective 23 August 2026**

Copyright © 2026 the Hexeris authors. All rights reserved.

> **In one sentence:** you may read, run, modify and test this software freely
> for evaluation; using it to actually operate a service, redistributing it, or
> shipping it under your own brand requires a commercial licence.

This is **not** an open-source licence as defined by the Open Source
Initiative, and it is not a free-software licence. Please read section 3 before
deploying anything.

---

## 1. Definitions

**"Software"** — the contents of this repository: source code, configuration,
deployment templates, documentation and assets, in any version.

**"You"** — the individual or legal entity exercising the rights granted here.
If you act within the scope of your employment or engagement, "You" includes
your employer or principal.

**"Licensor"** — the copyright holder named above.

**"Evaluation"** — installing, running, modifying and testing the Software to
assess its suitability, including internal demonstrations, functional and
security testing, load testing and proof-of-concept work.

**"Production Use"** — any use that is not Evaluation. In particular, operating
the Software so that it carries real communications of real people —
employees, contractors, customers or the public — is Production Use, whether or
not money changes hands and whether or not the deployment is called a "pilot".

**"White Label"** — distributing, offering or operating the Software under a
name, brand, logo or domain other than the Licensor's, whether for yourself or
for a third party.

---

## 2. What you may do without paying

Subject to section 3, the Licensor grants You a worldwide, royalty-free,
non-exclusive, non-transferable, revocable licence to:

1. **Read and study** the Software in full.
2. **Run** the Software for Evaluation, with no limit on the number of
   installations or the duration of the evaluation.
3. **Modify** the Software and create derivative works, for Evaluation.
4. **Test** the Software, including security testing of Your own installation,
   and publish the results of that testing (see section 7).
5. **Fork** this repository and keep Your changes, provided the fork remains
   subject to this licence and carries this file unchanged.

These rights are granted to You alone. They do not extend to Your customers,
and they do not travel with copies You give to anyone else.

---

## 3. What requires a commercial licence

You must obtain a separate written commercial licence from the Licensor before
You:

1. **Put the Software into Production Use**, as defined above — including a
   "pilot" that carries real correspondence.
2. **Redistribute** the Software or a derivative of it, in source or binary
   form, to any third party — including as part of a larger product, an
   appliance, a container image or a managed service.
3. **Offer the Software as a service** to anyone outside Your own organisation,
   whether hosted by You or by a third party on Your behalf.
4. **Use it White Label**, as defined above.
5. **Remove, obscure or replace** the Licensor's copyright notices, this file,
   or the attribution described in section 5, other than under a commercial
   licence that permits rebranding.

Commercial licences are available, including White Label terms and terms that
cover deployment on behalf of Your own customers. To enquire, open an issue in
this repository or contact the Licensor through the address published there.

---

## 4. The boundary between evaluation and production

Because that boundary is where honest people disagree, here it is plainly.

**This is Evaluation** — no commercial licence needed:

- A test instance with synthetic accounts and synthetic messages.
- A staging environment used by the project team to try the product.
- Load testing, penetration testing and disaster-recovery rehearsals against
  Your own installation.
- A demonstration to colleagues or to management.
- Building a modified version to see whether an integration would work.

**This is Production Use** — a commercial licence is required first:

- Any deployment where employees, contractors or customers send each other real
  messages, however few and however briefly.
- A "two-week pilot with one department" that carries real correspondence.
- Any deployment You charge for, bundle into a paid offering, or operate for a
  third party.
- An internal deployment that replaces or supplements an existing corporate
  messenger.

If Your case is not clearly on one side of that line, ask before deploying. The
Licensor would rather answer a question than pursue a dispute, and an enquiry
made in good faith before deployment will be treated as such.

---

## 5. Attribution

While You operate the Software under this licence, the interface must continue
to identify it as Hexeris, and copyright notices in the source must be left
intact. Changing the product name shown to users is White Label (section 3.4)
and requires a commercial licence — which is precisely what a White Label
licence grants.

Configuring `APP_NAME` for an evaluation instance is not a breach; shipping
that instance to users under another brand is.

---

## 6. Contributions

If You submit a contribution — a pull request, a patch or a suggestion — You
grant the Licensor a perpetual, worldwide, royalty-free, irrevocable licence to
use, modify, sublicense and distribute it as part of the Software, including
under the commercial licences described in section 3. You confirm that You have
the right to grant that licence, and that Your employer, if relevant, does not
hold a claim over the contribution that would prevent it.

You retain Your copyright in Your contribution. This is a licence to the
Licensor, not an assignment.

---

## 7. Security research

Security testing of Your own installation is expressly permitted, and so is
publishing what You find.

If You discover a vulnerability, please report it privately first — through a
GitHub security advisory on this repository — and allow a reasonable period for
a fix before publishing. Ninety days is reasonable; less may be, where the
issue is being exploited. This is a request, not a condition of the licence:
nothing here restricts Your right to disclose.

Do not test installations You do not operate. That is not a licence term; it is
the law in most jurisdictions.

---

## 8. Third-party components

The Software depends on third-party libraries, each under its own licence
(BSD-2-Clause, BSD-3-Clause, MIT and Apache-2.0 among them). Those licences
govern those components; this licence covers only the Software itself.

The dependency set is declared in `go.mod` and pinned in `go.sum`. Bundled web
fonts carry their own licences, stated in `web/fonts`.

---

## 9. Trademarks

This licence grants no rights in the name "Hexeris", the Hexeris logo, or any
other mark of the Licensor, beyond the attribution required by section 5. In
particular, it grants no right to suggest that a modified version is endorsed
by, or originates from, the Licensor.

---

## 10. No warranty

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT.

This matters more than boilerplate usually does, and the project documents its
own limits rather than hiding them. The Software is **not** end-to-end
encrypted: message bodies and files are encrypted at rest with a key held by
the server operator, which is a deliberate design decision explained in
`docs/security/SECURITY.md`. `ROADMAP.md` states what has been measured, what
has not, and what is missing. Read both before deciding what the Software is
fit for. Whether it suits Your regulatory obligations is Your assessment to
make, and Evaluation exists so that You can make it.

---

## 11. Limitation of liability

IN NO EVENT SHALL THE LICENSOR BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR ITS USE.

Where applicable law does not permit the exclusion of certain liability, this
section applies to the fullest extent that law allows.

---

## 12. Term and termination

This licence takes effect when You first exercise any right under it, and
continues until terminated.

It terminates automatically if You breach section 3. Rights granted under a
separate commercial licence are governed by that agreement and are unaffected.

If You cure a breach within 30 days of becoming aware of it — including by
obtaining the commercial licence You needed — this licence is reinstated as of
the date the breach began. A licence terminated a second time for the same
cause is not reinstated automatically.

On termination You must stop using the Software and destroy Your copies. Data
You created with it is Yours; nothing here gives the Licensor any claim to it.

---

## 13. Governing law

This licence is governed by the laws of the jurisdiction in which the Licensor
is established, without regard to conflict-of-laws rules. Where mandatory
consumer-protection law in Your jurisdiction gives You rights that this section
would remove, that law prevails.

---

## 14. Entire agreement

This file is the whole of the licence for use without payment. Nothing in the
README, the documentation, this repository's issues or any conversation
modifies it, except a written commercial licence signed by the Licensor — which
prevails over this file to the extent the two conflict.

If any provision is held unenforceable, the rest remains in force.
