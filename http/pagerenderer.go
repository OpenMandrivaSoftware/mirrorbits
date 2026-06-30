// Copyright (c) 2014-2019 Ludovic Fauvet
// Licensed under the MIT license

package http

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	. "github.com/etix/mirrorbits/config"
	"github.com/etix/mirrorbits/core"
	"github.com/etix/mirrorbits/mirrors"
)

var (
	// ErrTemplatesNotFound is returned cannot be loaded
	ErrTemplatesNotFound = errors.New("please set a valid path to the templates directory")
)

// resultsRenderer is the interface for all result renderers
type resultsRenderer interface {
	Write(ctx *Context, results *mirrors.Results) (int, error)
	Type() string
}

// JSONRenderer is used to render JSON formatted details about the current request
type JSONRenderer struct{}

// Type returns the type of renderer
func (w *JSONRenderer) Type() string {
	return "JSON"
}

// Write is used to write the result to the ResponseWriter
func (w *JSONRenderer) Write(ctx *Context, results *mirrors.Results) (statusCode int, err error) {

	if ctx.IsPretty() {
		output, err := json.MarshalIndent(results, "", "    ")
		if err != nil {
			return http.StatusInternalServerError, err
		}

		ctx.ResponseWriter().Header().Set("Content-Type", "application/json; charset=utf-8")
		ctx.ResponseWriter().Header().Set("Content-Length", strconv.Itoa(len(output)))
		ctx.ResponseWriter().Write(output)
	} else {
		ctx.ResponseWriter().Header().Set("Content-Type", "application/json; charset=utf-8")
		err = json.NewEncoder(ctx.ResponseWriter()).Encode(results)
		if err != nil {
			return http.StatusInternalServerError, err
		}
	}

	return http.StatusOK, nil
}

// RedirectRenderer is a basic renderer that redirects the user to the first mirror in the list
type RedirectRenderer struct{}

// Type returns the type of renderer
func (w *RedirectRenderer) Type() string {
	return "REDIRECT"
}

// Write is used to write the result to the ResponseWriter
func (w *RedirectRenderer) Write(ctx *Context, results *mirrors.Results) (statusCode int, err error) {
	if len(results.MirrorList) > 0 {
		ctx.ResponseWriter().Header().Set("Content-Type", "text/html; charset=utf-8")

		path := strings.TrimPrefix(results.FileInfo.Path, "/")

		mh := len(results.MirrorList)
		maxheaders := GetConfig().MaxLinkHeaders
		if mh > maxheaders+1 {
			mh = maxheaders + 1
		}

		if mh >= 1 {
			// Generate the header alternative links
			for i, m := range results.MirrorList[1:mh] {
				var countryCode string
				if len(m.CountryFields) > 0 {
					countryCode = strings.ToLower(m.CountryFields[0])
				}
				ctx.ResponseWriter().Header().Add("Link", fmt.Sprintf("<%s>; rel=duplicate; pri=%d; geo=%s", m.AbsoluteURL+path, i+1, countryCode))
			}
		}

		// Finally issue the redirect
		http.Redirect(ctx.ResponseWriter(), ctx.Request(), results.MirrorList[0].AbsoluteURL+path, http.StatusFound)
		return http.StatusFound, nil
	}
	// No mirror returned for this request
	http.NotFound(ctx.ResponseWriter(), ctx.Request())
	return http.StatusNotFound, nil
}

// Metalink 4.0 (RFC 5854) document structures. The XML namespace on the root
// element produces the required xmlns="urn:ietf:params:xml:ns:metalink".
type metalink struct {
	XMLName   xml.Name       `xml:"urn:ietf:params:xml:ns:metalink metalink"`
	Generator string         `xml:"generator,omitempty"`
	Published string         `xml:"published,omitempty"`
	Files     []metalinkFile `xml:"file"`
}

type metalinkFile struct {
	Name   string         `xml:"name,attr"`
	Size   int64          `xml:"size,omitempty"`
	Hashes []metalinkHash `xml:"hash,omitempty"`
	URLs   []metalinkURL  `xml:"url"`
}

type metalinkHash struct {
	Type  string `xml:"type,attr"`
	Value string `xml:",chardata"`
}

type metalinkURL struct {
	Location string `xml:"location,attr,omitempty"`
	Priority int    `xml:"priority,attr,omitempty"`
	Value    string `xml:",chardata"`
}

// MetalinkRenderer renders a Metalink 4.0 (RFC 5854) document listing the
// candidate mirrors for the requested file, ordered by preference, so that the
// client (dnf/librepo, aria2, ...) can perform the final mirror selection and
// failover itself instead of being redirected to a single mirror.
type MetalinkRenderer struct{}

// Type returns the type of renderer
func (w *MetalinkRenderer) Type() string {
	return "METALINK"
}

// Write is used to write the result to the ResponseWriter
func (w *MetalinkRenderer) Write(ctx *Context, results *mirrors.Results) (statusCode int, err error) {
	if len(results.MirrorList) == 0 {
		// No mirror returned for this request
		http.NotFound(ctx.ResponseWriter(), ctx.Request())
		return http.StatusNotFound, nil
	}

	// Path of the file relative to the repository root (no leading slash),
	// used to build the per-mirror URLs.
	path := strings.TrimPrefix(results.FileInfo.Path, "/")

	file := metalinkFile{
		// The "name" is the file basename, not the full path. librepo (dnf)
		// matches it by exact string equality against the name it expects
		// (e.g. "repomd.xml"), so a full path here would be ignored and the
		// metalink rejected. This also matches what Fedora's MirrorManager
		// emits and what aria2 uses as the output filename.
		Name: filepath.Base(path),
		Size: results.FileInfo.Size,
	}

	// Source file hashes, identical across all mirrors. Hash type names follow
	// the IANA registry as required by RFC 5854.
	if results.FileInfo.Sha256 != "" {
		file.Hashes = append(file.Hashes, metalinkHash{Type: "sha-256", Value: results.FileInfo.Sha256})
	}
	if results.FileInfo.Sha1 != "" {
		file.Hashes = append(file.Hashes, metalinkHash{Type: "sha-1", Value: results.FileInfo.Sha1})
	}
	if results.FileInfo.Md5 != "" {
		file.Hashes = append(file.Hashes, metalinkHash{Type: "md5", Value: results.FileInfo.Md5})
	}

	// Candidate mirrors, already ordered by preference by the selection engine.
	// priority 1 is the most preferred (RFC 5854 §4.1.6).
	for i, m := range results.MirrorList {
		var location string
		if len(m.CountryFields) > 0 {
			location = strings.ToLower(m.CountryFields[0])
		}
		file.URLs = append(file.URLs, metalinkURL{
			Location: location,
			Priority: i + 1,
			Value:    m.AbsoluteURL + path,
		})
	}

	doc := metalink{
		Generator: "mirrorbits/" + core.VERSION,
		Published: time.Now().UTC().Format(time.RFC3339),
		Files:     []metalinkFile{file},
	}

	output, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return http.StatusInternalServerError, err
	}

	ctx.ResponseWriter().Header().Set("Content-Type", "application/metalink4+xml; charset=utf-8")
	ctx.ResponseWriter().Header().Set("Content-Length", strconv.Itoa(len(xml.Header)+len(output)))
	ctx.ResponseWriter().Write([]byte(xml.Header))
	ctx.ResponseWriter().Write(output)

	return http.StatusOK, nil
}

// Metalink 3.0 (metalinker.org) document structures. This is the legacy format
// consumed by dnf/librepo, which does NOT understand the RFC 5854 (v4) layout:
// it expects <files>/<file>/<resources>/<url> with a "preference" attribute and
// hashes wrapped in <verification>. This is what Fedora's MirrorManager serves.
type metalink3 struct {
	XMLName   xml.Name        `xml:"http://www.metalinker.org/ metalink"`
	Version   string          `xml:"version,attr"`
	Type      string          `xml:"type,attr,omitempty"`
	Generator string          `xml:"generator,attr,omitempty"`
	Files     []metalink3File `xml:"files>file"`
}

type metalink3File struct {
	Name   string          `xml:"name,attr"`
	Size   int64           `xml:"size,omitempty"`
	Hashes []metalink3Hash `xml:"verification>hash,omitempty"`
	URLs   []metalink3URL  `xml:"resources>url"`
}

type metalink3Hash struct {
	Type  string `xml:"type,attr"`
	Value string `xml:",chardata"`
}

type metalink3URL struct {
	Protocol   string `xml:"protocol,attr,omitempty"`
	Type       string `xml:"type,attr,omitempty"`
	Location   string `xml:"location,attr,omitempty"`
	Preference int    `xml:"preference,attr,omitempty"`
	Value      string `xml:",chardata"`
}

// Metalink3Renderer renders a Metalink 3.0 document. This is the format dnf
// expects in a repo's metalink= directive (the url= entries point at each
// mirror's repomd.xml; the client derives the per-mirror baseurl from them and
// performs the final selection/failover itself).
type Metalink3Renderer struct{}

// Type returns the type of renderer
func (w *Metalink3Renderer) Type() string {
	return "METALINK3"
}

// Write is used to write the result to the ResponseWriter
func (w *Metalink3Renderer) Write(ctx *Context, results *mirrors.Results) (statusCode int, err error) {
	if len(results.MirrorList) == 0 {
		http.NotFound(ctx.ResponseWriter(), ctx.Request())
		return http.StatusNotFound, nil
	}

	path := strings.TrimPrefix(results.FileInfo.Path, "/")

	file := metalink3File{
		// Basename only: librepo matches it by exact equality (e.g. "repomd.xml")
		Name: filepath.Base(path),
		Size: results.FileInfo.Size,
	}

	// Metalink 3.0 uses dashless hash type names inside <verification>.
	if results.FileInfo.Sha256 != "" {
		file.Hashes = append(file.Hashes, metalink3Hash{Type: "sha256", Value: results.FileInfo.Sha256})
	}
	if results.FileInfo.Sha1 != "" {
		file.Hashes = append(file.Hashes, metalink3Hash{Type: "sha1", Value: results.FileInfo.Sha1})
	}
	if results.FileInfo.Md5 != "" {
		file.Hashes = append(file.Hashes, metalink3Hash{Type: "md5", Value: results.FileInfo.Md5})
	}

	// Candidate mirrors. In Metalink 3.0 "preference" is 0-100, higher is more
	// preferred (the opposite of v4's "priority"), so map the rank accordingly.
	for i, m := range results.MirrorList {
		var location string
		if len(m.CountryFields) > 0 {
			location = strings.ToLower(m.CountryFields[0])
		}
		proto := "http"
		if strings.HasPrefix(m.AbsoluteURL, "https://") {
			proto = "https"
		}
		preference := 100 - i
		if preference < 1 {
			preference = 1
		}
		file.URLs = append(file.URLs, metalink3URL{
			Protocol:   proto,
			Type:       proto,
			Location:   location,
			Preference: preference,
			Value:      m.AbsoluteURL + path,
		})
	}

	doc := metalink3{
		Version:   "3.0",
		Generator: "mirrorbits/" + core.VERSION,
		Files:     []metalink3File{file},
	}

	output, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return http.StatusInternalServerError, err
	}

	ctx.ResponseWriter().Header().Set("Content-Type", "application/metalink+xml; charset=utf-8")
	ctx.ResponseWriter().Header().Set("Content-Length", strconv.Itoa(len(xml.Header)+len(output)))
	ctx.ResponseWriter().Write([]byte(xml.Header))
	ctx.ResponseWriter().Write(output)

	return http.StatusOK, nil
}

// MirrorListRenderer is used to render the mirrorlist page using the HTML templates
type MirrorListRenderer struct{}

// Type returns the type of renderer
func (w *MirrorListRenderer) Type() string {
	return "MIRRORLIST"
}

// Write is used to write the result to the ResponseWriter
func (w *MirrorListRenderer) Write(ctx *Context, results *mirrors.Results) (statusCode int, err error) {
	if ctx.Templates().mirrorlist == nil {
		// No templates found for the mirrorlist
		return http.StatusInternalServerError, ErrTemplatesNotFound
	}
	// Sort the exclude reasons by message so they appear grouped
	sort.Sort(mirrors.ByExcludeReason{Mirrors: results.ExcludedList})

	// Create a temporary output buffer to render the page
	var buf bytes.Buffer

	ctx.ResponseWriter().Header().Set("Content-Type", "text/html; charset=utf-8")

	// Render the page into the buffer
	err = ctx.Templates().mirrorlist.ExecuteTemplate(&buf, "base", results)
	if err != nil {
		// Something went wrong, discard the buffer
		return http.StatusInternalServerError, err
	}

	// Write the buffer to the socket
	buf.WriteTo(ctx.ResponseWriter())
	return http.StatusOK, nil
}
