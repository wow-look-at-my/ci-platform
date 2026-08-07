package fakes

import "encoding/base64"

func encodeBase64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
