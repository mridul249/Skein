package accounts

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"golang.org/x/oauth2"

	"github.com/mridul60214/skein/internal/skerr"
)

const userinfoEndpoint = "https://openidconnect.googleapis.com/v1/userinfo"

// googleProfile is the minimum identity Skein needs from Google.
//
// Sub is the provider's stable account identifier and is the only field
// linking is keyed on. Email is a label for the UI: it can be changed, and
// treating it as identity is the account-takeover pattern Rules.md §2.4 bans.
type googleProfile struct {
	Sub   string `json:"sub"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

func (s *Service) fetchGoogleProfile(ctx context.Context, token *oauth2.Token) (googleProfile, error) {
	ctx, cancel := context.WithTimeout(ctx, providerTimeout)
	defer cancel()

	client := oauth2.NewClient(ctx, oauth2.StaticTokenSource(token))
	client.Timeout = providerTimeout

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, userinfoEndpoint, nil)
	if err != nil {
		return googleProfile{}, fmt.Errorf("build userinfo request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return googleProfile{}, fmt.Errorf("fetch google profile: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return googleProfile{}, skerr.Public(skerr.ErrUnauthorized,
			"Google would not confirm which account that was. Try connecting again.")
	}

	var p googleProfile
	// Bounded: a profile document is small, and an unbounded decode of a
	// provider response is still an unbounded decode.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&p); err != nil {
		return googleProfile{}, fmt.Errorf("decode google profile: %w", err)
	}
	if strings.TrimSpace(p.Sub) == "" {
		return googleProfile{}, skerr.Public(skerr.ErrUnauthorized,
			"Google did not return an account id. Try connecting again.")
	}
	if p.Email == "" {
		p.Email = "unknown@" + p.Sub
	}
	return p, nil
}
