// abridgedabridgedsummary combines all google groups abridged summaries in your inbox into one new email.
package main

import (
	"bufio"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/net/context"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	gmail "google.golang.org/api/gmail/v1"
)

type AbridgedAbridgedSummaryClient struct {
	svc *gmail.UsersService
}

func (c *AbridgedAbridgedSummaryClient) CombineAndArchiveThread(t *gmail.Thread) error {
	// todo: copy contents into structure

	//_, err := c.svc.Threads.Modify("me", t.Id, &gmail.ModifyThreadRequest{
	//	RemoveLabelIds: []string{"INBOX"},
	//}).Do()
	//return err
	return nil
}

func (c *AbridgedAbridgedSummaryClient) ForeachThread(q string, fn func(*gmail.Thread) error) error {
	pageToken := ""
	for {
		req := c.svc.Threads.List("me").Q(q)
		if pageToken != "" {
			req.PageToken(pageToken)
		}
		res, err := req.Do()
		if err != nil {
			return err
		}
		for _, t := range res.Threads {
			if err := fn(t); err != nil {
				return err
			}
		}
		if res.NextPageToken == "" {
			return nil
		}
		pageToken = res.NextPageToken
	}
}

// PopulateThread populates t with its full data. t.Id must be set initially.
func (c *AbridgedAbridgedSummaryClient) PopulateThread(t *gmail.Thread) error {
	req := c.svc.Threads.Get("me", t.Id).Format("full")
	tfull, err := req.Do()
	if err != nil {
		return err
	}
	*t = *tfull
	return nil
}

func main() {
	const OOB = "urn:ietf:wg:oauth:2.0:oob"
	conf := &oauth2.Config{
		ClientID: "789057330640-c7scp3t58rg4qi7jds2ddmd2tr3lnj6e.apps.googleusercontent.com", // proj: abridged-abridged-summary

		// https://developers.google.com/identity/protocols/OAuth2InstalledApp
		// says: "The client ID and client secret obtained
		// from the Developers Console are embedded in the
		// source code of your application. In this context,
		// the client secret is obviously not treated as a
		// secret."
		ClientSecret: "e6aqd0Zf9eRGCRsJYaL9TW2s",

		Endpoint:    google.Endpoint,
		RedirectURL: OOB,
		Scopes:      []string{gmail.MailGoogleComScope},
	}

	cacheDir := filepath.Join(userCacheDir(), "abridgedabridgedsummary")
	gmailTokenFile := filepath.Join(cacheDir, "gmail.token")

	slurp, err := ioutil.ReadFile(gmailTokenFile)
	var ts oauth2.TokenSource
	if err == nil {
		f := strings.Fields(strings.TrimSpace(string(slurp)))
		if len(f) == 2 {
			ts = conf.TokenSource(context.Background(), &oauth2.Token{
				AccessToken:  f[0],
				TokenType:    "Bearer",
				RefreshToken: f[1],
				Expiry:       time.Unix(1, 0),
			})
			if _, err := ts.Token(); err != nil {
				log.Printf("Cached token invalid: %v", err)
				ts = nil
			}
		}
	}

	if ts == nil {
		authCode := conf.AuthCodeURL("state")
		log.Printf("Go to %v", authCode)
		io.WriteString(os.Stdout, "Enter code> ")

		bs := bufio.NewScanner(os.Stdin)
		if !bs.Scan() {
			os.Exit(1)
		}
		code := strings.TrimSpace(bs.Text())
		t, err := conf.Exchange(context.Background(), code)
		if err != nil {
			log.Fatal(err)
		}
		os.MkdirAll(cacheDir, 0700)
		ioutil.WriteFile(gmailTokenFile, []byte(t.AccessToken+" "+t.RefreshToken), 0600)
		ts = conf.TokenSource(context.Background(), t)
	}

	client := oauth2.NewClient(context.Background(), ts)
	svc, err := gmail.New(client)
	if err != nil {
		log.Fatal(err)
	}

	aasc := &AbridgedAbridgedSummaryClient{
		svc: svc.Users,
	}
	n := 0
	if err := aasc.ForeachThread("in:inbox", func(t *gmail.Thread) error {
		if err := aasc.PopulateThread(t); err != nil {
			return err
		}
		n++
		log.Printf("Thread %d (%v) = %v", n, t.Id, headerValue(t.Messages[0], "Subject"))
		if !aasc.AbridgedSummaryThread(t) {
			return nil
		}
		log.Printf("combining and archiving...")
		return aasc.CombineAndArchiveThread(t)
	}); err != nil {
		log.Fatal(err)
	}
}

func (c *AbridgedAbridgedSummaryClient) AbridgedSummaryThread(t *gmail.Thread) bool {
	if len(t.Messages) != 1 {
		return false
	}
	message := t.Messages[0]
	mpart := message.Payload
	if mpart == nil {
		return false
	}
	var subjectIsAbridgedSummary bool
	var fromGoogleGroups bool
	for _, mph := range mpart.Headers {
		if mph.Name == "X-Google-Group-Id" {
			fromGoogleGroups = true
		} else if mph.Name == "Subject" && strings.Contains(strings.ToLower(mph.Value), "abridged summary") {
			subjectIsAbridgedSummary = true
		}
	}
	return fromGoogleGroups && subjectIsAbridgedSummary
}

func headerValue(m *gmail.Message, header string) string {
	mpart := m.Payload
	if mpart == nil {
		return ""
	}
	for _, mph := range mpart.Headers {
		if mph.Name == header {
			return mph.Value
		}
	}
	return ""
}

func userCacheDir() string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(HomeDir(), "Library", "Caches")
	case "windows":
		// TODO: use Application Data instead, or something?

		// Per http://technet.microsoft.com/en-us/library/cc749104(v=ws.10).aspx
		// these should both exist. But that page overwhelms me. Just try them
		// both. This seems to work.
		for _, ev := range []string{"TEMP", "TMP"} {
			if v := os.Getenv(ev); v != "" {
				return ev
			}
		}
		panic("No Windows TEMP or TMP environment variables found")
	}
	if xdg := os.Getenv("XDG_CACHE_HOME"); xdg != "" {
		return xdg
	}
	return filepath.Join(HomeDir(), ".cache")
}

func HomeDir() string {
	if runtime.GOOS == "windows" {
		return os.Getenv("HOMEDRIVE") + os.Getenv("HOMEPATH")
	}
	return os.Getenv("HOME")
}
