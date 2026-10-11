// Package qr renders a BR Code (pixCopiaECola) as a PNG, base64-encoded.
package qr

import (
	"encoding/base64"

	"github.com/skip2/go-qrcode"
)

func PNG(text string) (string, error) {
	png, err := qrcode.Encode(text, qrcode.Medium, 256)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(png), nil
}
