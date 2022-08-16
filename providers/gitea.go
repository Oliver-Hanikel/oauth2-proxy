package providers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/oauth2-proxy/oauth2-proxy/v7/pkg/apis/options"
	"github.com/oauth2-proxy/oauth2-proxy/v7/pkg/apis/sessions"
	"github.com/oauth2-proxy/oauth2-proxy/v7/pkg/logger"
	"github.com/oauth2-proxy/oauth2-proxy/v7/pkg/requests"
)

// GiteaProvider represents an Gitea based Identity Provider
type GiteaProvider struct {
	*ProviderData
	ValidateURL *url.URL
	ApiURL      *url.URL
	Org         string
	Team        string
	Repo        string
	Token       string
	Users       []string
}

var _ Provider = (*GiteaProvider)(nil)

const (
	giteaProviderName = "Gitea"
	giteaDefaultScope = "user:email"
)

var (
	// Default Login URL for Gitea.
	// Pre-parsed URL of https://try.gitea.io/login/oauth/authorize.
	giteaDefaultLoginURL = &url.URL{
		Scheme: "https",
		Host:   "try.gitea.io",
		Path:   "/login/oauth/authorize",
	}

	// Default Redeem URL for Gitea.
	// Pre-parsed URL of https://try.gitea.io/login/oauth/access_token.
	giteaDefaultRedeemURL = &url.URL{
		Scheme: "https",
		Host:   "try.gitea.io",
		Path:   "/login/oauth/access_token",
	}

	// Default Validation URL for Gitea.
	// ValidationURL is not the API Base URL, because the base returns 404.
	// https://github.com/oauth2-proxy/oauth2-proxy/issues/1636
	// Other API requests are based the ApiURL which is based off of this (eg to fetch users/groups).
	// Pre-parsed URL of https://try.gitea.io/api/v1/user.
	giteaDefaultValidateURL = &url.URL{
		Scheme: "https",
		Host:   "try.gitea.io",
		Path:   "/api/v1/user",
	}
)

// NewGiteaProvider initiates a new GiteaProvider
func NewGiteaProvider(p *ProviderData, opts options.GiteaOptions) *GiteaProvider {
	p.setProviderDefaults(providerDefaults{
		name:        giteaProviderName,
		loginURL:    giteaDefaultLoginURL,
		redeemURL:   giteaDefaultRedeemURL,
		profileURL:  nil,
		validateURL: giteaDefaultValidateURL,
		scope:       giteaDefaultScope,
	})

	provider := &GiteaProvider{ProviderData: p}

	provider.setOrgTeam(opts.Org, opts.Team)
	provider.setRepo(opts.Repo, opts.Token)
	provider.setUsers(opts.Users)
	return provider
}

func makeGiteaHeader(accessToken string) http.Header {
	// extra headers required by the Gitea API when making authenticated requests
	extraHeaders := map[string]string{}
	return makeAuthorizationHeader(tokenTypeToken, accessToken, extraHeaders)
}

func (p *GiteaProvider) getApiURL() *url.URL {
	fmt.Printf("\n%s\n\n", p.ApiURL)
	if p.ApiURL == nil {
		p.ApiURL, _ = url.Parse(path.Dir(p.ValidateURL.String()))
	}
	return p.ApiURL
}

// setOrgTeam adds Gitea org reading parameters to the OAuth2 scope
func (p *GiteaProvider) setOrgTeam(org, team string) {
	p.Org = org
	p.Team = team
	if org != "" || team != "" {
		p.Scope += " read:org"
	}
}

// setRepo configures the target repository and optional token to use
func (p *GiteaProvider) setRepo(repo, token string) {
	p.Repo = repo
	p.Token = token
}

// setUsers configures allowed usernames
func (p *GiteaProvider) setUsers(users []string) {
	p.Users = users
}

// EnrichSession updates the User & Email after the initial Redeem
func (p *GiteaProvider) EnrichSession(ctx context.Context, s *sessions.SessionState) error {
	err := p.getEmail(ctx, s)
	if err != nil {
		return err
	}
	return p.getUser(ctx, s)
}

// ValidateSession validates the AccessToken
func (p *GiteaProvider) ValidateSession(ctx context.Context, s *sessions.SessionState) bool {
	return validateToken(ctx, p, s.AccessToken, makeGiteaHeader(s.AccessToken))
}

func (p *GiteaProvider) hasOrg(ctx context.Context, accessToken string) (bool, error) {
	// https://developer.gitea.com/v3/orgs/#list-your-organizations

	var orgs []struct {
		Login string `json:"login"`
	}

	type orgsPage []struct {
		Login string `json:"login"`
	}

	pn := 1
	for {
		params := url.Values{
			"per_page": {"100"},
			"page":     {strconv.Itoa(pn)},
		}

		endpoint := &url.URL{
			Scheme:   p.getApiURL().Scheme,
			Host:     p.getApiURL().Host,
			Path:     path.Join(p.getApiURL().Path, "/user/orgs"),
			RawQuery: params.Encode(),
		}

		var op orgsPage
		err := requests.New(endpoint.String()).
			WithContext(ctx).
			WithHeaders(makeGiteaHeader(accessToken)).
			Do().
			UnmarshalInto(&op)
		if err != nil {
			return false, err
		}

		if len(op) == 0 {
			break
		}

		orgs = append(orgs, op...)
		pn++
	}

	presentOrgs := make([]string, 0, len(orgs))
	for _, org := range orgs {
		if p.Org == org.Login {
			logger.Printf("Found Gitea Organization: %q", org.Login)
			return true, nil
		}
		presentOrgs = append(presentOrgs, org.Login)
	}

	logger.Printf("Missing Organization:%q in %v", p.Org, presentOrgs)
	return false, nil
}

func (p *GiteaProvider) hasOrgAndTeam(ctx context.Context, accessToken string) (bool, error) {
	// https://developer.gitea.com/v3/orgs/teams/#list-user-teams

	var teams []struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
		Org  struct {
			Login string `json:"login"`
		} `json:"organization"`
	}

	type teamsPage []struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
		Org  struct {
			Login string `json:"login"`
		} `json:"organization"`
	}

	pn := 1
	last := 0
	for {
		params := url.Values{
			"per_page": {"100"},
			"page":     {strconv.Itoa(pn)},
		}

		endpoint := &url.URL{
			Scheme:   p.getApiURL().Scheme,
			Host:     p.getApiURL().Host,
			Path:     path.Join(p.getApiURL().Path, "/user/teams"),
			RawQuery: params.Encode(),
		}

		// bodyclose cannot detect that the body is being closed later in requests.Into,
		// so have to skip the linting for the next line.
		// nolint:bodyclose
		result := requests.New(endpoint.String()).
			WithContext(ctx).
			WithHeaders(makeGiteaHeader(accessToken)).
			Do()
		if result.Error() != nil {
			return false, result.Error()
		}

		if last == 0 {
			// link header may not be obtained
			// When paging is not required and all data can be retrieved with a single call

			// Conditions for obtaining the link header.
			// 1. When paging is required (Example: When the data size is 100 and the page size is 99 or less)
			// 2. When it exceeds the paging frame (Example: When there is only 10 records but the second page is called with a page size of 100)

			// link header at not last page
			// <https://api.gitea.com/user/teams?page=1&per_page=100>; rel="prev", <https://api.gitea.com/user/teams?page=1&per_page=100>; rel="last", <https://api.gitea.com/user/teams?page=1&per_page=100>; rel="first"
			// link header at last page (doesn't exist last info)
			// <https://api.gitea.com/user/teams?page=3&per_page=10>; rel="prev", <https://api.gitea.com/user/teams?page=1&per_page=10>; rel="first"

			link := result.Headers().Get("Link")
			rep1 := regexp.MustCompile(`(?s).*\<https://api.gitea.com/user/teams\?page=(.)&per_page=[0-9]+\>; rel="last".*`)
			i, converr := strconv.Atoi(rep1.ReplaceAllString(link, "$1"))

			// If the last page cannot be taken from the link in the http header, the last variable remains zero
			if converr == nil {
				last = i
			}
		}

		var tp teamsPage
		if err := result.UnmarshalInto(&tp); err != nil {
			return false, err
		}
		if len(tp) == 0 {
			break
		}

		teams = append(teams, tp...)

		if pn == last {
			break
		}
		if last == 0 {
			break
		}

		pn++
	}

	var hasOrg bool
	presentOrgs := make(map[string]bool)
	var presentTeams []string
	for _, team := range teams {
		presentOrgs[team.Org.Login] = true
		if p.Org == team.Org.Login {
			hasOrg = true
			ts := strings.Split(p.Team, ",")
			for _, t := range ts {
				if t == team.Slug {
					logger.Printf("Found Gitea Organization:%q Team:%q (Name:%q)", team.Org.Login, team.Slug, team.Name)
					return true, nil
				}
			}
			presentTeams = append(presentTeams, team.Slug)
		}
	}
	if hasOrg {
		logger.Printf("Missing Team:%q from Org:%q in teams: %v", p.Team, p.Org, presentTeams)
	} else {
		var allOrgs []string
		for org := range presentOrgs {
			allOrgs = append(allOrgs, org)
		}
		logger.Printf("Missing Organization:%q in %#v", p.Org, allOrgs)
	}
	return false, nil
}

func (p *GiteaProvider) hasRepo(ctx context.Context, accessToken string) (bool, error) {
	// https://developer.gitea.com/v3/repos/#get-a-repository

	type permissions struct {
		Pull bool `json:"pull"`
		Push bool `json:"push"`
	}

	type repository struct {
		Permissions permissions `json:"permissions"`
		Private     bool        `json:"private"`
	}

	endpoint := &url.URL{
		Scheme: p.getApiURL().Scheme,
		Host:   p.getApiURL().Host,
		Path:   path.Join(p.getApiURL().Path, "/repos/", p.Repo),
	}

	var repo repository
	err := requests.New(endpoint.String()).
		WithContext(ctx).
		WithHeaders(makeGiteaHeader(accessToken)).
		Do().
		UnmarshalInto(&repo)
	if err != nil {
		return false, err
	}

	// Every user can implicitly pull from a public repo, so only grant access
	// if they have push access or the repo is private and they can pull
	return repo.Permissions.Push || (repo.Private && repo.Permissions.Pull), nil
}

func (p *GiteaProvider) hasUser(ctx context.Context, accessToken string) (bool, error) {
	// https://developer.gitea.com/v3/users/#get-the-authenticated-user

	var user struct {
		Login string `json:"login"`
		Email string `json:"email"`
	}

	endpoint := &url.URL{
		Scheme: p.getApiURL().Scheme,
		Host:   p.getApiURL().Host,
		Path:   path.Join(p.getApiURL().Path, "/user"),
	}

	err := requests.New(endpoint.String()).
		WithContext(ctx).
		WithHeaders(makeGiteaHeader(accessToken)).
		Do().
		UnmarshalInto(&user)
	if err != nil {
		return false, err
	}

	if p.isVerifiedUser(user.Login) {
		return true, nil
	}
	return false, nil
}

func (p *GiteaProvider) isCollaborator(ctx context.Context, username, accessToken string) (bool, error) {
	//https://developer.gitea.com/v3/repos/collaborators/#check-if-a-user-is-a-collaborator

	endpoint := &url.URL{
		Scheme: p.getApiURL().Scheme,
		Host:   p.getApiURL().Host,
		Path:   path.Join(p.getApiURL().Path, "/repos/", p.Repo, "/collaborators/", username),
	}
	result := requests.New(endpoint.String()).
		WithContext(ctx).
		WithHeaders(makeGiteaHeader(accessToken)).
		Do()
	if result.Error() != nil {
		return false, result.Error()
	}

	if result.StatusCode() != 204 {
		return false, fmt.Errorf("got %d from %q %s",
			result.StatusCode(), endpoint.String(), result.Body())
	}

	logger.Printf("got %d from %q %s", result.StatusCode(), endpoint.String(), result.Body())

	return true, nil
}

// getEmail updates the SessionState Email
func (p *GiteaProvider) getEmail(ctx context.Context, s *sessions.SessionState) error {

	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}

	// If usernames are set, check that first
	verifiedUser := false
	if len(p.Users) > 0 {
		var err error
		verifiedUser, err = p.hasUser(ctx, s.AccessToken)
		if err != nil {
			return err
		}
		// org and repository options are not configured
		if !verifiedUser && p.Org == "" && p.Repo == "" {
			return errors.New("missing gitea user")
		}
	}
	// If a user is verified by username options, skip the following restrictions
	if !verifiedUser {
		if p.Org != "" {
			if p.Team != "" {
				if ok, err := p.hasOrgAndTeam(ctx, s.AccessToken); err != nil || !ok {
					return err
				}
			} else {
				if ok, err := p.hasOrg(ctx, s.AccessToken); err != nil || !ok {
					return err
				}
			}
		} else if p.Repo != "" && p.Token == "" { // If we have a token we'll do the collaborator check in GetUserName
			if ok, err := p.hasRepo(ctx, s.AccessToken); err != nil || !ok {
				return err
			}
		}
	}

	endpoint := &url.URL{
		Scheme: p.getApiURL().Scheme,
		Host:   p.getApiURL().Host,
		Path:   path.Join(p.getApiURL().Path, "/user/emails"),
	}
	err := requests.New(endpoint.String()).
		WithContext(ctx).
		WithHeaders(makeGiteaHeader(s.AccessToken)).
		Do().
		UnmarshalInto(&emails)
	if err != nil {
		return err
	}

	for _, email := range emails {
		if email.Verified {
			if email.Primary {
				s.Email = email.Email
				return nil
			}
		}
	}

	return nil
}

// getUser updates the SessionState User
func (p *GiteaProvider) getUser(ctx context.Context, s *sessions.SessionState) error {
	var user struct {
		Login string `json:"login"`
		Email string `json:"email"`
	}

	endpoint := &url.URL{
		Scheme: p.getApiURL().Scheme,
		Host:   p.getApiURL().Host,
		Path:   path.Join(p.getApiURL().Path, "/user"),
	}

	err := requests.New(endpoint.String()).
		WithContext(ctx).
		WithHeaders(makeGiteaHeader(s.AccessToken)).
		Do().
		UnmarshalInto(&user)
	if err != nil {
		return err
	}

	// Now that we have the username we can check collaborator status
	if !p.isVerifiedUser(user.Login) && p.Org == "" && p.Repo != "" && p.Token != "" {
		if ok, err := p.isCollaborator(ctx, user.Login, p.Token); err != nil || !ok {
			return err
		}
	}

	s.User = user.Login
	return nil
}

// isVerifiedUser
func (p *GiteaProvider) isVerifiedUser(username string) bool {
	for _, u := range p.Users {
		if username == u {
			return true
		}
	}
	return false
}
