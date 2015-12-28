// abridgedabridgedsummary combines all google groups abridged summaries in your inbox into one new email.
package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"net/mail"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/context"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	gmail "google.golang.org/api/gmail/v1"
)

type AbridgedAbridgedSummaryClient struct {
	svc    *gmail.UsersService
	Groups []Group
}

type Group struct {
	Name             string
	ThreadListingURL string
	Threads          []Thread
}

type Thread struct {
	Subject string
	URL     string
	Updates []Update
}

type Update struct {
	RawTRInnerHTML template.HTML
}

var (
	groupNameFromURL = regexp.MustCompile(`^.*#!forum/(.*)/topics$`)
)

func GroupFromEmail(email []byte) (Group, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewBuffer(email))
	if err != nil {
		return Group{}, err
	}
	firstLink := doc.Find("a")
	href, _ := firstLink.Attr("href")
	href = strings.TrimSpace(href)
	groupNameResult := groupNameFromURL.FindAllStringSubmatch(href, -1)
	group := Group{
		Name:             groupNameResult[0][1],
		ThreadListingURL: href,
	}

	// parse threads
	doc.Find("a").Each(func(_ int, threadAnchor *goquery.Selection) {
		nameAttr, _ := threadAnchor.Attr("name")
		if strings.Index(nameAttr, "group_thread") != 0 {
			return
		}
		var thread Thread
		thread.Subject = strings.TrimSpace(threadAnchor.Next().Text())
		thread.URL, _ = threadAnchor.Next().Find("a").Attr("href")
		// parse updates
		threadAnchor.Next().Next().Find("tr").Each(func(j int, updateTr *goquery.Selection) {
			html, err := updateTr.Html()
			if err != nil {
				panic(err)
			}
			thread.Updates = append(thread.Updates, Update{template.HTML(html)})
		})
		group.Threads = MergeThread(group.Threads, thread)
	})

	return group, nil
}

func MergeGroup(into []Group, group Group) []Group {
	foundit := -1
	for i, g := range into {
		if g.Name == group.Name {
			foundit = i
			break
		}
	}
	if foundit == -1 {
		return append(into, group)
	}
	for _, thread := range group.Threads {
		into[foundit].Threads = MergeThread(into[foundit].Threads, thread)
	}
	return into
}

func MergeThread(into []Thread, thread Thread) []Thread {
	foundit := -1
	for i, t := range into {
		if t.URL == thread.URL {
			foundit = i
			break
		}
	}
	if foundit == -1 {
		return append(into, thread)
	}

	// assume thread.Updates is in chrono order, but that all thread.Updates came before existing
	// TODO: add timestamp to Update so that this isn't so fragile
	into[foundit].Updates = append(thread.Updates, into[foundit].Updates...)
	return into
}

func (c *AbridgedAbridgedSummaryClient) CombineAndArchiveThread(t *gmail.Thread) error {
	message := t.Messages[0]
	for _, part := range message.Payload.Parts {
		if part.MimeType == "text/html" {
			data, err := base64.URLEncoding.DecodeString(part.Body.Data)
			if err != nil {
				return err
			}
			//log.Print(string(data))
			group, err := GroupFromEmail(data)
			if err != nil {
				return err
			}
			c.Groups = MergeGroup(c.Groups, group)
		}
	}

	_, err := c.svc.Threads.Modify("me", t.Id, &gmail.ModifyThreadRequest{
		RemoveLabelIds: []string{"INBOX"},
	}).Do()
	return err
}

func (c *AbridgedAbridgedSummaryClient) SendSummary() error {
	// render the email
	slurp, err := ioutil.ReadFile("combined.tmpl")
	if err != nil {
		return err
	}
	t := template.Must(template.New("").Parse(string(slurp)))

	var buf bytes.Buffer
	//test, _ := os.Create("test.html")
	//defer test.Close()
	if err := t.Execute(&buf, map[string]interface{}{"Groups": c.Groups}); err != nil {
		return err
	}

	// get profile so we can correctly set from/to
	profile, err := c.svc.GetProfile("me").Do()
	if err != nil {
		return err
	}
	from := mail.Address{"", profile.EmailAddress}
	to := mail.Address{"", profile.EmailAddress}
	header := make(map[string]string)
	header["From"] = from.String()
	header["To"] = to.String()
	header["Subject"] = "Abridged Abridged Summary"
	header["MIME-Version"] = "1.0"
	header["Content-Type"] = "text/html; charset=\"utf-8\""
	header["Content-Transfer-Encoding"] = "base64"

	var msg string
	for k, v := range header {
		msg += fmt.Sprintf("%s: %s\r\n", k, v)
	}
	msg += "\r\n" + buf.String()

	gmsg := gmail.Message{
		Raw: base64.URLEncoding.EncodeToString([]byte(msg)),
	}

	_, err = c.svc.Messages.Send("me", &gmsg).Do()
	if err != nil {
		log.Fatalf("em %v, err %v", gmsg, err)
		return err
	}

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
	if err := aasc.SendSummary(); err != nil {
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
