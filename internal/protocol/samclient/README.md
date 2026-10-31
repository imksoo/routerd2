# SAM leaf enrollment

The `sam_client` service submits a configured signed claim through the existing
routerd HTTP enrollment client, fetches the admitted RR topology, and renews it
using the server receipt. It uses the configured token file and TLS trust; import
never reads the token. The original receipt expiry is converted once to the local
boot clock. Failed requests cannot extend it. Optional direct peers are published
only when every configured endpoint attests the current claim and agrees on the
complete direct group. A missing or invalid direct group leaves an independently
valid RR set available until its original expiry.

The worker reports `samclientconfig.Snapshot` through the existing service state
API. Its private `services/NAME.sam.sock` provides `GET /v1/status` and explicit
`POST /v1/revoke`. Withdrawal first saves the identity with the existing atomic
file writer and publishes an empty observation, then asks every configured RR to
revoke it. A failed remote withdrawal is retried without registering that identity
again. Worker shutdown is not a revoke. A deliberately changed signed identity
can enroll after the old identity was revoked.

`go test -race ./internal/protocol/samclient ./internal/legacyconfig` covers the
HTTP client lifecycle, two-RR direct agreement, renewal, deadline expiration,
withdrawal delivery and restart. `make check-sam-enrollment
OLD_ROUTERD=/absolute/path/to/linux/routerd` runs against an independent actual
routerd server: signed admission, SQLite persistence, topology projection and
revocation. Its sandbox flag disables unrelated kernel writers; the HTTP and
admission store are real. The fixture uses only loopback and temporary files.

The service runtime's snapshot admission must be integrated separately. Applying
learned RR/direct peers to IPIP/BGP and implementing the routerd2 RR server are
not established by these checks. This service does not import the old dynamic
configuration journal or own network resources.
