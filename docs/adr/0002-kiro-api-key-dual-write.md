# Kiro API Key dual-write on API-key Accounts

```yaml
status: accepted
```

Wave C adds API-key Accounts (`AuthMethod=api_key`, secret `ksk_…`) alongside OAuth Accounts. Most outbound paths already read `AccessToken` as the bearer. Storing the Kiro API Key only in a new field would miss call sites; storing only in `AccessToken` would erase the distinction from OAuth tokens and break refresh/profile assumptions.

**Decision:** `KiroApiKey` is the source of truth for an API-key Account. Every write path that sets or updates that secret must dual-write `AccessToken = KiroApiKey`. `IsApiKeyCredential()` is true when `KiroApiKey` is set or `AuthMethod` is `api_key`/`apikey`; those Accounts skip OAuth refresh and profile-ARN resolution and send bearer with API_KEY token type.

**Naming:** Gateway API Key (`sk-…`, client→proxy) is not an API-key Account and must not share fields/UI copy with Kiro API Key.

**MVP surface:** add one Account, import one JSON (`authMethod=api_key`, `kiroApiKey`), mask `ksk_…` in logs/UI. Bulk `apikeys-batch` is deferred.

**Consequences:** Invariants are easy to violate if a future edit path sets only one of the two fields—tests must assert dual-write. Config on disk gains `kiroApiKey`; backups and admin export must keep masking. Migrating away later means either keeping the mirror forever or auditing every bearer/header path.
