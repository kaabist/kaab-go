# Changelog

The SDK's version is the Kaabist API's major.minor (`info.version` of the OpenAPI document) and a
patch of its own; a breaking change of the API comes only with `/v2` (EPIC-34 T6).

## Unreleased

- Removed: research and search (EPIC-77). `c.Research`, `c.Search`, `WaitForResearch`,
  `ResearchWaitOptions`, `Error.Research`, the research types and the `kaab research` and
  `kaab search` commands are gone; the API answers those endpoints `410 feature_removed`.
- `Jobs.Get(ctx, id, params, opts...)` takes `*client.GetJobParams` (`SinceStatus`); pass `nil`
  for none. Not yet published, so no released code breaks.
- `Jobs.Wait` long-polls against the status it last saw (`since_status`).
- A body that fails mid-read is retried after the usual backoff, and a cancelled context stops it.

## 1.38.0

- First release of the client and the `kaab` CLI: every operation of API 1.38 as a method, generated from the OpenAPI document;
  retries with backoff and `Retry-After`, automatic idempotency keys on job creation, `Prefer: wait`,
  typed errors, list iteration across pages, `jobs.wait` with input and attention handlers, and
  webhook signature verification.
