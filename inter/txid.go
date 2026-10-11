package inter

// ValidTxid is Bacen's rule for a receiver-chosen txid: [a-zA-Z0-9]{26,35}.
func ValidTxid(s string) bool {
	if len(s) < 26 || len(s) > 35 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
