package automation

import (
	"bytes"
	"fmt"
	"html/template"
	"strings"
	txttpl "text/template"

	"github.com/knadh/listmonk/models"
	"golang.org/x/net/html"
)

// ConfirmationData exposes the same native confirmation/preference URLs as the
// system email. No arbitrary URL from a subscription request is accepted.
type ConfirmationData struct {
	Subscriber models.Subscriber
	Lists      []models.List
	OptinURL   string
	UnsubURL   string
}

func RenderConfirmation(subject, body string, data ConfirmationData) (string, string, error) {
	if strings.TrimSpace(subject) == "" || strings.ContainsAny(subject, "\r\n") {
		return "", "", fmt.Errorf("subject is required and must be a single line")
	}
	t, err := txttpl.New("subject").Option("missingkey=error").Parse(subject)
	if err != nil {
		return "", "", err
	}
	h, err := template.New("confirmation").Option("missingkey=error").Parse(body)
	if err != nil {
		return "", "", err
	}
	var s, b bytes.Buffer
	if err = t.Execute(&s, data); err != nil {
		return "", "", err
	}
	if strings.ContainsAny(s.String(), "\r\n") {
		return "", "", fmt.Errorf("rendered subject must be a single line")
	}
	if err = h.Execute(&b, data); err != nil {
		return "", "", err
	}
	if !HasLink(b.String(), data.OptinURL) {
		return "", "", fmt.Errorf("include an anchor with href=\"{{ .OptinURL }}\"")
	}
	return s.String(), b.String(), nil
}

// HasLink requires an actual anchor pointing to the generated native URL.
func HasLink(body, expected string) bool {
	// A visible anchor must retain the actual generated link. A placeholder in a
	// comment or text node is not sufficient to enable confirmation.
	z := html.NewTokenizer(strings.NewReader(body))
	linked := false
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		if tt != html.StartTagToken {
			continue
		}
		n, more := z.TagName()
		if string(n) != "a" {
			continue
		}
		for more {
			k, v, m := z.TagAttr()
			more = m
			if string(k) == "href" && string(v) == expected {
				linked = true
			}
		}
	}
	return linked
}
