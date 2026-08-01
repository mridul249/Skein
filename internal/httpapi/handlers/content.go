package handlers

import (
	"fmt"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/mridul249/Skein/internal/skerr"
	"github.com/mridul249/Skein/internal/storage"
)

// inlineAllowlist is the complete set of types Skein will render in the
// browser. Rules.md §2.3.
//
// The list is short on purpose. Anything not on it is served as
// application/octet-stream with Content-Disposition: attachment, so a file
// that happens to contain HTML is downloaded rather than executed on the app's
// origin. text/html and any *+xml are absent and must stay absent: they are
// script execution contexts, and a user-uploaded one served inline is stored
// XSS with the victim's own session attached.
var inlineAllowlist = map[string]bool{
	"image/png":       true,
	"image/jpeg":      true,
	"image/gif":       true,
	"image/webp":      true,
	"image/avif":      true,
	"video/mp4":       true,
	"video/webm":      true,
	"audio/mpeg":      true,
	"audio/ogg":       true,
	"audio/wav":       true,
	"application/pdf": true,
	"text/plain":      true,
}

// disposition decides how a file's bytes are served.
//
// sniffed comes from http.DetectContentType over the first bytes of the actual
// content — never from the client's declared MIME, which is a claim and was
// the reference project's stored-XSS vector.
func disposition(sniffed string) (contentType string, inline bool) {
	base, _, err := mime.ParseMediaType(sniffed)
	if err != nil {
		base = strings.ToLower(strings.TrimSpace(strings.Split(sniffed, ";")[0]))
	}
	base = strings.ToLower(base)

	if !inlineAllowlist[base] {
		return "application/octet-stream", false
	}

	// text/plain needs a charset or the browser guesses, and a guess of
	// UTF-16 turns some byte sequences into markup.
	if base == "text/plain" {
		return "text/plain; charset=utf-8", true
	}
	return base, true
}

// contentDispositionHeader builds a header that survives non-ASCII filenames
// without letting a name break out of the quoted string.
func contentDispositionHeader(kind, filename string) string {
	// Quotes and backslashes would terminate the quoted-string early; both
	// are stripped rather than escaped, because a filename containing them
	// is not worth the parsing risk.
	ascii := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' || r == '\\' {
			return -1
		}
		if r > 0x7e {
			return '_'
		}
		return r
	}, filename)
	if ascii == "" {
		ascii = "download"
	}

	// RFC 6266: the plain parameter for old clients, filename* for the rest.
	return fmt.Sprintf(`%s; filename="%s"; filename*=UTF-8''%s`,
		kind, ascii, urlEncodePath(filename))
}

// urlEncodePath percent-encodes for the filename* parameter. url.PathEscape
// leaves several characters that are valid in a path but not in this header.
func urlEncodePath(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		isUnreserved := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '.' || c == '_' || c == '~'
		if isUnreserved {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0x0f])
	}
	return b.String()
}

// parseRange parses a single-range Range header.
//
// Multi-range requests are refused rather than half-implemented: answering
// them requires multipart/byteranges, no browser media element sends one, and
// a wrong implementation silently corrupts a download. Refusing means the
// client retries without the header.
//
// Every number is parsed with an error return. Rules.md §2.6: a malformed
// header from the wire must never panic.
func parseRange(header string, size int64) (*storage.ByteRange, error) {
	if header == "" {
		return nil, nil
	}
	// RFC 9110: no range is satisfiable on a zero-length representation.
	// Without this the suffix form ("bytes=-1") clamps to a zero-length
	// range and looks like a successful 206 of nothing.
	if size == 0 {
		return nil, errRangeUnsatisfiable
	}
	const prefix = "bytes="
	if !strings.HasPrefix(header, prefix) {
		return nil, errRangeUnsatisfiable
	}
	spec := strings.TrimSpace(header[len(prefix):])
	if strings.Contains(spec, ",") {
		// Not satisfiable as a single range; the caller answers 416 and
		// the client retries whole.
		return nil, errRangeUnsatisfiable
	}

	startTxt, endTxt, found := strings.Cut(spec, "-")
	if !found {
		return nil, errRangeUnsatisfiable
	}
	startTxt = strings.TrimSpace(startTxt)
	endTxt = strings.TrimSpace(endTxt)

	switch {
	case startTxt == "" && endTxt == "":
		return nil, errRangeUnsatisfiable

	case startTxt == "":
		// "bytes=-500": the final 500 bytes.
		n, err := strconv.ParseInt(endTxt, 10, 64)
		if err != nil || n <= 0 {
			return nil, errRangeUnsatisfiable
		}
		if n > size {
			n = size
		}
		return &storage.ByteRange{Start: size - n, Length: n}, nil

	case endTxt == "":
		// "bytes=500-": from 500 to the end.
		start, err := strconv.ParseInt(startTxt, 10, 64)
		if err != nil || start < 0 || start >= size {
			return nil, errRangeUnsatisfiable
		}
		return &storage.ByteRange{Start: start, Length: size - start}, nil

	default:
		start, err := strconv.ParseInt(startTxt, 10, 64)
		if err != nil || start < 0 {
			return nil, errRangeUnsatisfiable
		}
		end, err := strconv.ParseInt(endTxt, 10, 64)
		if err != nil || end < start {
			return nil, errRangeUnsatisfiable
		}
		if start >= size {
			return nil, errRangeUnsatisfiable
		}
		// The end is inclusive on the wire and may point past the last
		// byte, which is legal and means "to the end".
		if end >= size {
			end = size - 1
		}
		return &storage.ByteRange{Start: start, Length: end - start + 1}, nil
	}
}

var errRangeUnsatisfiable = skerr.Public(skerr.ErrValidation, "That byte range is not satisfiable.")

// writeRangeNotSatisfiable answers 416 with the Content-Range the spec
// requires, so the client learns the real size and can retry correctly.
func writeRangeNotSatisfiable(w http.ResponseWriter, size int64) {
	w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(size, 10))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
	_, _ = w.Write([]byte(`{"error":"range_not_satisfiable","message":"That byte range is not satisfiable."}`))
}
