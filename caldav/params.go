package caldav

import (
	"bytes"
	"strings"
)

// maxParamValue caps each iCalendar parameter value before go-ical decodes a request body.
// go-ical builds parameter values a byte at a time, so decoding costs a value's length squared:
// a 512 KiB value inside an otherwise small body is minutes of CPU. Real values (TZID, CN,
// ALTREP) are far shorter.
const maxParamValue = 1 << 10

// longParams reports a content line with a parameter value over maxParamValue, reading lines
// and parameters exactly as go-ical's decoder does (decoder.go: readContinuedLine,
// decodeContentLine, decodeParam), so no syntax the decoder accepts escapes the check. Linear.
func longParams(body []byte) bool {
	var line []byte
	for len(body) > 0 {
		phys := body
		if i := bytes.IndexByte(body, '\n'); i >= 0 {
			phys, body = body[:i], body[i+1:]
		} else {
			body = nil
		}
		line = append(line, bytes.TrimSuffix(phys, []byte("\r"))...) // bufio.ReadLine drops only that \r
		if len(body) > 0 && (body[0] == ' ' || body[0] == '\t') {
			body = body[1:] // a fold: the decoder drops the space and joins the next line
			continue
		}
		if longParam(string(line)) {
			return true
		}
		line = line[:0]
	}
	return false
}

func longParam(s string) bool {
	i := strings.IndexAny(s, ";:")
	if i < 0 {
		return false
	}
	for s = s[i:]; strings.HasPrefix(s, ";"); {
		// The parameter name runs to the next '=' anywhere on the line, a ':' included.
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			return false
		}
		s = s[eq+1:]
		for {
			var n int
			if strings.HasPrefix(s, `"`) {
				end := strings.IndexByte(s[1:], '"')
				if end < 0 {
					return len(s)-1 > maxParamValue // read to the end, then refused
				}
				n, s = end, s[end+2:]
			} else {
				end := strings.IndexAny(s, "\";,:")
				if end < 0 {
					end = len(s)
				}
				n, s = end, s[end:]
				if strings.HasPrefix(s, `"`) {
					return n > maxParamValue // read up to the quote, then refused
				}
			}
			if n > maxParamValue {
				return true
			}
			if s == "" || s[0] != ',' {
				break // ';' next parameter, ':' the value; anything else the decoder refuses
			}
			s = s[1:]
		}
	}
	return false
}
