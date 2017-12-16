// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package model

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

var searchTermPuncStart = regexp.MustCompile(`^[^\pL\d\s#"]+`)
var searchTermPuncEnd = regexp.MustCompile(`[^\pL\d\s*"]+$`)

type SearchParams struct {
	Terms           string
	IsHashtag       bool
	InChannels      []string
	FromUsers       []string
	WithAttachments []string
	OrTerms         bool
	Count           int
}

func (o *SearchParams) ToJson() string {
	b, err := json.Marshal(o)
	if err != nil {
		return ""
	} else {
		return string(b)
	}
}

var searchFlags = [...]string{"from", "channel", "in", "attachment", "file", "count"}

func splitWords(text string) []string {
	words := []string{}

	foundQuote := false
	location := 0
	for i, char := range text {
		if char == '"' {
			if foundQuote {
				// Grab the quoted section
				word := text[location : i+1]
				words = append(words, word)
				foundQuote = false
				location = i + 1
			} else {
				words = append(words, strings.Fields(text[location:i])...)
				foundQuote = true
				location = i
			}
		}
	}

	words = append(words, strings.Fields(text[location:])...)

	return words
}

func parseSearchFlags(input []string) ([]string, [][2]string) {
	words := []string{}
	flags := [][2]string{}

	skipNextWord := false
	for i, word := range input {
		if skipNextWord {
			skipNextWord = false
			continue
		}

		isFlag := false

		if colon := strings.Index(word, ":"); colon != -1 {
			flag := word[:colon]
			value := word[colon+1:]

			for _, searchFlag := range searchFlags {
				// check for case insensitive equality
				if strings.EqualFold(flag, searchFlag) {
					if value != "" {
						flags = append(flags, [2]string{searchFlag, value})
						isFlag = true
					} else if i < len(input)-1 {
						flags = append(flags, [2]string{searchFlag, input[i+1]})
						skipNextWord = true
						isFlag = true
					}

					if isFlag {
						break
					}
				}
			}
		}

		if !isFlag {
			// trim off surrounding punctuation (note that we leave trailing asterisks to allow wildcards)
			word = searchTermPuncStart.ReplaceAllString(word, "")
			word = searchTermPuncEnd.ReplaceAllString(word, "")

			// and remove extra pound #s
			word = hashtagStart.ReplaceAllString(word, "#")

			if len(word) != 0 {
				words = append(words, word)
			}
		}
	}

	return words, flags
}

func ParseSearchParams(text string) []*SearchParams {
	words, flags := parseSearchFlags(splitWords(text))

	hashtagTermList := []string{}
	plainTermList := []string{}

	for _, word := range words {
		if validHashtag.MatchString(word) {
			hashtagTermList = append(hashtagTermList, word)
		} else {
			plainTermList = append(plainTermList, word)
		}
	}

	hashtagTerms := strings.Join(hashtagTermList, " ")
	plainTerms := strings.Join(plainTermList, " ")

	inChannels := []string{}
	fromUsers := []string{}
	withAttachments := []string{}
	count := -1

	for _, flagPair := range flags {
		flag := flagPair[0]
		value := flagPair[1]

		if flag == "in" || flag == "channel" {
			inChannels = append(inChannels, value)
		} else if flag == "from" {
			fromUsers = append(fromUsers, value)
		} else if flag == "attachment" || flag == "file" {
			withAttachments = append(withAttachments, value)
		} else if flag == "count" {
			count_, err := strconv.ParseUint(value, 10, 16)
			if err == nil {
				count = int(count_)
			}
		}
	}

	paramsList := []*SearchParams{}

	if len(plainTerms) > 0 {
		paramsList = append(paramsList, &SearchParams{
			Terms:           plainTerms,
			IsHashtag:       false,
			InChannels:      inChannels,
			FromUsers:       fromUsers,
			WithAttachments: withAttachments,
			Count:           count,
		})
	}

	if len(hashtagTerms) > 0 {
		paramsList = append(paramsList, &SearchParams{
			Terms:           hashtagTerms,
			IsHashtag:       true,
			InChannels:      inChannels,
			FromUsers:       fromUsers,
			WithAttachments: withAttachments,
			Count:           count,
		})
	}

	// special case for when no terms are specified but we still have a filter
	if len(plainTerms) == 0 && len(hashtagTerms) == 0 && (len(inChannels) != 0 || len(fromUsers) != 0 || len(withAttachments) != 0 ) {
		paramsList = append(paramsList, &SearchParams{
			Terms:           "",
			IsHashtag:       false,
			InChannels:      inChannels,
			FromUsers:       fromUsers,
			WithAttachments: withAttachments,
			Count:           count,
		})
	}

	return paramsList
}
