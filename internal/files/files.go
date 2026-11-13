// Package files moves a member's files between the Workspace and a sandbox workspace (ADR-0001 D15; platform ADR-0002
// F3, F4). The bytes never pass through the model or the gateway: a tool call carries a ticket the Workspace signed for
// this run (a read ticket names one file, a write ticket lets the run deliver files), idp-host put it into the arguments
// after the model's turn, and this package presents it to the Workspace's internal file endpoint, which verifies it.
// What comes in or goes out is checked here as well: a type the platform accepts, no macros, no zip bomb.
package files

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

// File is a file of the Workspace as it describes it.
type File struct {
	Ref       string `json:"ref"`
	Name      string `json:"name"`
	MediaType string `json:"mediaType"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
}

// Client talks to the Workspace's internal file endpoint (IDP_SANDBOX_FILES_URL, e.g. http://workspace:8560/internal/v1/files).
type Client struct {
	BaseURL  string
	MaxBytes int64
	HTTP     *http.Client
}

// NewClient returns a client that never uses a proxy (the Workspace is on the private network).
func NewClient(base string, maxBytes int64) *Client {
	return &Client{BaseURL: strings.TrimRight(base, "/"), MaxBytes: maxBytes,
		HTTP: &http.Client{Timeout: 120 * time.Second, Transport: &http.Transport{Proxy: nil}}}
}

var (
	ErrTicket   = errors.New("the file reference is not valid for this run (use the attachment or output reference you were given)")
	ErrNotYours = errors.New("the file reference belongs to someone else")
	ErrTooLarge = errors.New("the file is larger than the transfer limit")
)

// Claims is what this side reads from a ticket, without verifying it (the Workspace does): whose it is.
type Claims struct {
	Tenant  string `json:"tenant_id"`
	Subject string `json:"sub"`
	Kind    string `json:"kind"` // read | write
}

// Peek decodes a ticket's claims. The sandbox only uses them to refuse a ticket that is not the caller's before it reaches
// the Workspace (defence in depth: the Workspace verifies the signature, the audience and the expiry).
func Peek(ticket string) (Claims, error) {
	parts := strings.Split(ticket, ".")
	if len(parts) != 3 || len(ticket) > 8192 {
		return Claims{}, ErrTicket
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, ErrTicket
	}
	var c Claims
	if json.Unmarshal(raw, &c) != nil || c.Subject == "" || c.Tenant == "" || (c.Kind != "read" && c.Kind != "write") {
		return Claims{}, ErrTicket
	}
	return c, nil
}

// For checks that a ticket is of the given kind and belongs to owner ("tenant/subject", the sandbox's workspace owner).
func For(ticket, kind, owner string) error {
	c, err := Peek(ticket)
	if err != nil || c.Kind != kind {
		return ErrTicket
	}
	if c.Tenant+"/"+c.Subject != owner {
		return ErrNotYours
	}
	return nil
}

// Get fetches the file a read ticket names.
func (c *Client) Get(ctx context.Context, ticket string) (File, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/content", nil)
	if err != nil {
		return File{}, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+ticket)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return File{}, nil, fmt.Errorf("the Workspace did not answer: %w", err)
	}
	defer resp.Body.Close()
	if err := status(resp); err != nil {
		return File{}, nil, err
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, c.MaxBytes+1))
	if err != nil {
		return File{}, nil, err
	}
	if int64(len(data)) > c.MaxBytes {
		return File{}, nil, ErrTooLarge
	}
	f := File{Ref: resp.Header.Get("Idp-File-Ref"), MediaType: resp.Header.Get("Content-Type"), Size: int64(len(data)), SHA256: resp.Header.Get("Idp-File-Sha256")}
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil {
		f.Name = params["filename"]
	}
	return f, data, nil
}

// Put delivers a file with a write ticket; the Workspace records it as the run's deliverable and answers with its ref.
func (c *Client) Put(ctx context.Context, ticket, name, mediaType string, data []byte) (File, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL, bytes.NewReader(data))
	if err != nil {
		return File{}, err
	}
	req.Header.Set("Authorization", "Bearer "+ticket)
	req.Header.Set("Content-Type", mediaType)
	req.Header.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return File{}, fmt.Errorf("the Workspace did not answer: %w", err)
	}
	defer resp.Body.Close()
	if err := status(resp); err != nil {
		return File{}, err
	}
	var f File
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&f); err != nil || f.Ref == "" {
		return File{}, errors.New("the Workspace's answer has no file reference")
	}
	return f, nil
}

func status(resp *http.Response) error {
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return ErrTicket
	case resp.StatusCode == http.StatusRequestEntityTooLarge:
		return ErrTooLarge
	}
	var p struct {
		Error struct{ Message string } `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<14)).Decode(&p)
	if p.Error.Message != "" {
		return fmt.Errorf("the Workspace refused the file: %s", p.Error.Message)
	}
	return fmt.Errorf("the Workspace refused the file (HTTP %d)", resp.StatusCode)
}

// Types the platform moves in and out, by extension, with what their content must begin with.
var types = map[string]struct {
	mediaType string
	magic     []string // one of these prefixes; empty = text
	ooxml     bool
}{
	".docx": {"application/vnd.openxmlformats-officedocument.wordprocessingml.document", []string{"PK\x03\x04"}, true},
	".xlsx": {"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", []string{"PK\x03\x04"}, true},
	".pptx": {"application/vnd.openxmlformats-officedocument.presentationml.presentation", []string{"PK\x03\x04"}, true},
	".pdf":  {"application/pdf", []string{"%PDF-"}, false},
	".png":  {"image/png", []string{"\x89PNG\r\n\x1a\n"}, false},
	".jpg":  {"image/jpeg", []string{"\xff\xd8\xff"}, false},
	".jpeg": {"image/jpeg", []string{"\xff\xd8\xff"}, false},
	".csv":  {"text/csv", nil, false},
	".txt":  {"text/plain", nil, false},
	".md":   {"text/markdown", nil, false},
}

// MediaType returns the media type of a name the platform accepts ("" when it does not).
func MediaType(name string) string { return types[strings.ToLower(path.Ext(name))].mediaType }

// Limits of an Office file's zip, against a bomb or a mangled package.
const (
	maxEntries      = 10000
	maxUnpacked     = 256 << 20
	maxRatio        = 200
	maxNameBytes    = 512
	accepted        = ".docx .xlsx .pptx .pdf .png .jpg .jpeg .csv .txt .md"
	macroExtensions = ".docm .xlsm .pptm .dotm .xltm .potm .ppsm .xlsb .xlam .ppam"
)

// Check refuses what the platform does not move: a type outside the list, a macro-enabled format, content that is not what
// its name says, a zip bomb, a path that climbs out, or a macro project hidden in a .docx/.xlsx/.pptx.
func Check(name string, data []byte) error {
	ext := strings.ToLower(path.Ext(name))
	if strings.Contains(" "+macroExtensions+" ", " "+ext+" ") {
		return fmt.Errorf("%s files can carry macros and are not accepted", ext)
	}
	t, ok := types[ext]
	if !ok {
		return fmt.Errorf("only these file types are accepted: %s", accepted)
	}
	if t.magic == nil {
		if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
			return fmt.Errorf("%s is not a text file", name)
		}
		return nil
	}
	okMagic := false
	for _, m := range t.magic {
		okMagic = okMagic || bytes.HasPrefix(data, []byte(m))
	}
	if !okMagic {
		return fmt.Errorf("%s is not a %s file", name, ext)
	}
	if !t.ooxml {
		return nil
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("%s is not a valid %s package", name, ext)
	}
	if len(zr.File) > maxEntries {
		return fmt.Errorf("%s has too many parts", name)
	}
	var unpacked uint64
	for _, f := range zr.File {
		n := f.Name
		if len(n) > maxNameBytes || strings.HasPrefix(n, "/") || strings.Contains(n, "\\") || strings.Contains("/"+n+"/", "/../") {
			return fmt.Errorf("%s has a part with an unsafe name", name)
		}
		if strings.Contains(strings.ToLower(n), "vbaproject.bin") || strings.HasSuffix(strings.ToLower(n), ".bin") && strings.Contains(strings.ToLower(n), "vba") {
			return fmt.Errorf("%s carries a macro project and is not accepted", name)
		}
		unpacked += f.UncompressedSize64
		if f.CompressedSize64 > 0 && f.UncompressedSize64/f.CompressedSize64 > maxRatio && f.UncompressedSize64 > 1<<20 {
			return fmt.Errorf("%s has a part that unpacks to far more than its size", name)
		}
	}
	if unpacked > maxUnpacked {
		return fmt.Errorf("%s unpacks to more than %d MiB", name, maxUnpacked>>20)
	}
	if _, err := url.PathUnescape(name); err != nil {
		return fmt.Errorf("%s is not a valid file name", name)
	}
	return nil
}

// SafeName keeps a delivered file's name to one path element of printable characters.
func SafeName(name string) string {
	name = path.Base(strings.ReplaceAll(strings.TrimSpace(name), "\\", "/"))
	var b strings.Builder
	for _, r := range name {
		if r >= 0x20 && r != 0x7f && !(r >= 0x80 && r < 0xa0) && r != 0x2028 && r != 0x2029 && !strings.ContainsRune(`/:*?"<>|`, r) {
			b.WriteRune(r)
		}
	}
	out := strings.TrimLeft(strings.TrimSpace(b.String()), ".")
	if len(out) > 200 {
		ext := path.Ext(out)
		if len(ext) > 20 {
			ext = ""
		}
		cut := 200 - len(ext)
		for cut > 0 && !utf8.RuneStart(out[cut]) { // never half a character
			cut--
		}
		out = out[:cut] + ext
	}
	return out
}
