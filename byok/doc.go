// Package byok is the provider-neutral seam of "bring your own key": an
// organization's own bank or PSP account, reached with the organization's own
// credentials (billing spec 2026-10-11 § 4.4). Money is in centavos, dates are
// civil dates. Implementations: api-commons/inter. Test double: byok/byoktest.
//
// Nothing here reads SSM, the environment or a cache: credentials are passed
// per call, so one process can serve many organizations.
package byok
