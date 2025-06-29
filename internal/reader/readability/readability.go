// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package readability // import "miniflux.app/v2/internal/reader/readability"

import (
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"

	"miniflux.app/v2/internal/urllib"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"
)

const (
	defaultTagsToScore = "section,h2,h3,h4,h5,h6,p,td,pre,div"
)

var (
	divToPElementsRegexp = regexp.MustCompile(`(?i)<(?:a|blockquote|dl|div|img|ol|p|pre|table|ul)[ />]`)

	strongCandidates  = [...]string{"popupbody", "-ad", "g-plus"}
	maybeCandidate    = [...]string{"and", "article", "body", "column", "main", "shadow"}
	unlikelyCandidate = [...]string{"banner", "breadcrumbs", "combx", "comment", "community", "cover-wrap", "disqus", "extra", "foot", "header", "legends", "menu", "modal", "related", "remark", "replies", "rss", "shoutbox", "sidebar", "skyscraper", "social", "sponsor", "supplemental", "ad-break", "agegate", "pagination", "pager", "popup", "yom-remote"}

	negativeRegexp = regexp.MustCompile(`hid|banner|combx|comment|com-|contact|foot|masthead|media|meta|modal|outbrain|promo|related|scroll|share|shoutbox|sidebar|skyscraper|sponsor|shopping|tags|tool|widget|byline|author|dateline|writtenby`)
	positiveRegexp = regexp.MustCompile(`article|body|content|entry|hentry|h-entry|main|page|pagination|post|text|blog|story`)
)

type candidate struct {
	selection *goquery.Selection
	score     float32
}

func (c *candidate) Node() *html.Node {
	return c.selection.Get(0)
}

func (c *candidate) String() string {
	id, _ := c.selection.Attr("id")
	class, _ := c.selection.Attr("class")

	switch {
	case id != "" && class != "":
		return fmt.Sprintf("%s#%s.%s => %f", c.Node().DataAtom, id, class, c.score)
	case id != "":
		return fmt.Sprintf("%s#%s => %f", c.Node().DataAtom, id, c.score)
	case class != "":
		return fmt.Sprintf("%s.%s => %f", c.Node().DataAtom, class, c.score)
	}

	return fmt.Sprintf("%s => %f", c.Node().DataAtom, c.score)
}

type candidateList map[*html.Node]*candidate

func (c candidateList) String() string {
	var output []string
	for _, candidate := range c {
		output = append(output, candidate.String())
	}

	return strings.Join(output, ", ")
}

// ExtractContent returns relevant content.
func ExtractContent(page io.Reader) (baseURL string, extractedContent string, err error) {
	document, err := goquery.NewDocumentFromReader(page)
	if err != nil {
		return "", "", err
	}

	if hrefValue, exists := document.FindMatcher(goquery.Single("head base")).Attr("href"); exists {
		hrefValue = strings.TrimSpace(hrefValue)
		if urllib.IsAbsoluteURL(hrefValue) {
			baseURL = hrefValue
		}
	}

	document.Find("script,style").Remove()

	transformMisusedDivsIntoParagraphs(document)
	removeUnlikelyCandidates(document)

	candidates := getCandidates(document)
	topCandidate := getTopCandidate(document, candidates)

	slog.Debug("Readability parsing",
		slog.String("base_url", baseURL),
		slog.Any("candidates", candidates),
		slog.Any("topCandidate", topCandidate),
	)

	extractedContent = getArticle(topCandidate, candidates)
	return baseURL, extractedContent, nil
}

// Now that we have the top candidate, look through its siblings for content that might also be related.
// Things like preambles, content split by ads that we removed, etc.
func getArticle(topCandidate *candidate, candidates candidateList) string {
	var output strings.Builder
	output.WriteString("<div>")
	siblingScoreThreshold := max(10, topCandidate.score/5)

	topCandidate.selection.Siblings().Union(topCandidate.selection).Each(func(i int, s *goquery.Selection) {
		append := false
		tag := "div"
		node := s.Get(0)

		if node == topCandidate.Node() {
			append = true
		} else if c, ok := candidates[node]; ok && c.score >= siblingScoreThreshold {
			append = true
		} else if s.Is("p") {
			tag = node.Data
			linkDensity := getLinkDensity(s)
			content := s.Text()
			contentLength := len(content)

			if contentLength >= 80 {
				if linkDensity < .25 {
					append = true
				}
			} else {
				if linkDensity == 0 {
					if containsSentence(content) {
						append = true
					}
				}
			}
		}

		if append {
			html, _ := s.Html()
			output.WriteString("<" + tag + ">" + html + "</" + tag + ">")
		}
	})

	output.WriteString("</div>")
	return output.String()
}
func shouldRemoveCandidate(str string) bool {
	str = strings.ToLower(str)

	// Those candidates have no false-positives, no need to check against `maybeCandidate`
	for _, strong := range strongCandidates {
		if strings.Contains(str, strong) {
			return true
		}
	}

	for _, unlikely := range unlikelyCandidate {
		if strings.Contains(str, unlikely) {
			// Do we have a false positive?
			for _, maybe := range maybeCandidate {
				if strings.Contains(str, maybe) {
					return false
				}
			}

			// Nope, it's a true positive!
			return true
		}
	}
	return false
}

func removeUnlikelyCandidates(document *goquery.Document) {
	document.Find("*").Each(func(i int, s *goquery.Selection) {
		if s.Length() == 0 || s.Get(0).Data == "html" || s.Get(0).Data == "body" {
			return
		}

		// Don't remove elements within code blocks (pre or code tags)
		if s.Closest("pre, code").Length() > 0 {
			return
		}

		if class, ok := s.Attr("class"); ok && shouldRemoveCandidate(class) {
			s.Remove()
		} else if id, ok := s.Attr("id"); ok && shouldRemoveCandidate(id) {
			s.Remove()
		}
	})
}

func getTopCandidate(document *goquery.Document, candidates candidateList) *candidate {
	var best *candidate

	for _, c := range candidates {
		if best == nil {
			best = c
		} else if best.score < c.score {
			best = c
		}
	}

	if best == nil {
		best = &candidate{document.Find("body"), 0}
	}

	return best
}

// Loop through all paragraphs, and assign a score to them based on how content-y they look.
// Then add their score to their parent node.
// A score is determined by things like number of commas, class names, etc.
// Maybe eventually link density.
func getCandidates(document *goquery.Document) candidateList {
	candidates := make(candidateList)

	document.Find(defaultTagsToScore).Each(func(i int, s *goquery.Selection) {
		text := s.Text()

		// If this paragraph is less than 25 characters, don't even count it.
		if len(text) < 25 {
			return
		}

		parent := s.Parent()
		parentNode := parent.Get(0)

		grandParent := parent.Parent()
		var grandParentNode *html.Node
		if grandParent.Length() > 0 {
			grandParentNode = grandParent.Get(0)
		}

		if _, found := candidates[parentNode]; !found {
			candidates[parentNode] = scoreNode(parent)
		}

		if grandParentNode != nil {
			if _, found := candidates[grandParentNode]; !found {
				candidates[grandParentNode] = scoreNode(grandParent)
			}
		}

		// Add a point for the paragraph itself as a base.
		contentScore := float32(1.0)

		// Add points for any commas within this paragraph.
		contentScore += float32(strings.Count(text, ",") + 1)

		// For every 100 characters in this paragraph, add another point. Up to 3 points.
		contentScore += float32(min(len(text)/100.0, 3))

		candidates[parentNode].score += contentScore
		if grandParentNode != nil {
			candidates[grandParentNode].score += contentScore / 2.0
		}
	})

	// Scale the final candidates score based on link density. Good content
	// should have a relatively small link density (5% or less) and be mostly
	// unaffected by this operation
	for _, candidate := range candidates {
		candidate.score *= (1 - getLinkDensity(candidate.selection))
	}

	return candidates
}

func scoreNode(s *goquery.Selection) *candidate {
	c := &candidate{selection: s, score: 0}

	switch s.Get(0).DataAtom.String() {
	case "div":
		c.score += 5
	case "pre", "td", "blockquote", "img":
		c.score += 3
	case "address", "ol", "ul", "dl", "dd", "dt", "li", "form":
		c.score -= 3
	case "h1", "h2", "h3", "h4", "h5", "h6", "th":
		c.score -= 5
	}

	c.score += getClassWeight(s)
	return c
}

// Get the density of links as a percentage of the content
// This is the amount of text that is inside a link divided by the total text in the node.
func getLinkDensity(s *goquery.Selection) float32 {
	textLength := len(s.Text())

	if textLength == 0 {
		return 0
	}

	linkLength := len(s.Find("a").Text())

	return float32(linkLength) / float32(textLength)
}

// Get an elements class/id weight. Uses regular expressions to tell if this
// element looks good or bad.
func getClassWeight(s *goquery.Selection) float32 {
	weight := 0

	if class, ok := s.Attr("class"); ok {
		class = strings.ToLower(class)
		if negativeRegexp.MatchString(class) {
			weight -= 25
		} else if positiveRegexp.MatchString(class) {
			weight += 25
		}
	}

	if id, ok := s.Attr("id"); ok {
		id = strings.ToLower(id)
		if negativeRegexp.MatchString(id) {
			weight -= 25
		} else if positiveRegexp.MatchString(id) {
			weight += 25
		}
	}

	return float32(weight)
}

func transformMisusedDivsIntoParagraphs(document *goquery.Document) {
	document.Find("div").Each(func(i int, s *goquery.Selection) {
		html, _ := s.Html()
		if !divToPElementsRegexp.MatchString(html) {
			node := s.Get(0)
			node.Data = "p"
		}
	})
}

func containsSentence(content string) bool {
	return strings.HasSuffix(content, ".") || strings.Contains(content, ". ")
}
