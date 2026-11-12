package spotify

import "testing"

// TestPKCEChallengeMatchesRFC7636 is the published Appendix B vector. It pins
// the S256 transform and the unpadded base64url encoding.
func TestPKCEChallengeMatchesRFC7636(t *testing.T) {
	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	const challenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	if got := challengeFor(verifier); got != challenge {
		t.Fatalf("challengeFor = %q, want %q", got, challenge)
	}
}
