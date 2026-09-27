# Detect-only profiles

User authorization: add detection without masking for known noncritical debugging
data, then update the installed router. Existing default remains masking.

Design: profile `mode` is `mask` (also omitted) or `detect`; reject unknown values.
Bindings keep the same priority. Detect uses the configured filters on supported
text/tool/schema fields, retains only category counts, and returns the original
request unchanged. It creates no aliases or session dictionaries, does not restore
the response, and does not certify that a request is free of sensitive data.
Binary attachment contents and opaque thinking are not inspected. Count configured
withheld attachments separately. Encoded detection knows only current-request
secrets; no prior session is loaded. Existing transport/header/size/history
restrictions for globally enabled privacy still apply.

API: profile `mode?: mask|detect`; resolution includes `mode`. Lab keeps `mode`
for text/json format and adds `operation?: mask|detect`. Detect preview returns
`operation: detect`, unchanged `output`, `detected` category counts, empty `id`
and no restoration handle. `masked` is empty and `roundtrip` false. Lab state adds
`detects` (runs), `detected` (counts), observation `operation: detect` and `findings`.
Traffic state adds `detected` (requests) and `findings` (category counts), separate
from protected/bypassed/restored. Counts must not retain text or plaintext values.

Implementation:
- [x] Core pure detection, profile validation/default and snapshot mode selection.
- [x] Lab, aggregate observability and real HTTP unchanged-body regression tests.
- [x] UI mode selector, clear exposure notice, profile/single-filter detection,
      separate findings and no restoration affordance for detect results.
- [x] Independent review, focused/race/full tests and browser validation.
- [x] Install merged build with rollback binary and verify live service/UI.
      Installed `1c059a4`; exact mask/restore and detect API smoke plus live
      Chromium detect/clear passed. Traffic filtering remains disabled.
      Deployment evidence and first-attempt readiness timeout are recorded in
      `docs/privacy-traffic.md`.

UI work may run independently against this API contract while the parent handles
Go/runtime integration. Subagent-driven-development is used for this independent
UI task; the final review covers both parts. No profile is enabled in production
as part of installation: no production rules have been configured yet.
