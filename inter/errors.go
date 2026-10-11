package inter

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	"gopkg.aoctech.app/api-commons/byok"
)

// Problem is Bacen's RFC 7807 body. Only Title and the violation field names
// are ever surfaced: Detail can echo the request (a CPF, a key).
type Problem struct {
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail"`
	Violacoes []struct {
		Propriedade string `json:"propriedade"`
	} `json:"violacoes"`
}

func problemTitle(raw []byte) string {
	var p Problem
	if json.Unmarshal(raw, &p) != nil {
		return ""
	}
	s := p.Title
	for _, v := range p.Violacoes {
		s += " [" + v.Propriedade + "]"
	}
	return s
}

func statusErr(method, path string, code int, raw []byte) error {
	var base error
	switch {
	case code == 401:
		base = byok.ErrUnauthorized
	case code == 403:
		base = byok.ErrScopesRefused
	case code == 404:
		base = byok.ErrNotFound
	case code == 409:
		base = byok.ErrConcluded
	case code == 429:
		base = byok.ErrRateLimited
	case code >= 500:
		base = byok.ErrUnavailable
	default:
		base = byok.ErrInvalidRequest
	}
	return fmt.Errorf("%w: %s %s %d %s", base, method, path, code, problemTitle(raw))
}

// sanitize drops the URL (it can carry a query) from a transport error.
func sanitize(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
