// Copyright 2023 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package recon contains code for recon.
package recon

import (
	"context"
	"sort"
	"strings"
	"sync"

	pb "github.com/datacommonsorg/mixer/internal/proto"
	"github.com/datacommonsorg/mixer/internal/store"
	"github.com/datacommonsorg/mixer/internal/store/files"
)

const (
	maxPlaceCandidates = 15
	// When the number of places is larger than maxPlaceCandidates, firstly pick maxPlaceCandidates
	// places, for the rest, only pick the places with
	// population >= minPopulationOverMaxPlaceCandidates.
	minPopulationOverMaxPlaceCandidates = 2000
)

// RecognizePlaces implements API for Mixer.RecognizePlaces.
func RecognizePlaces(
	ctx context.Context,
	in *pb.RecognizePlacesRequest,
	store *store.Store,
	resolveBogusName bool,
) (*pb.RecognizePlacesResponse, error) {
	pr := &placeRecognition{
		recogPlaceStore:  store.RecogPlaceStore,
		resolveBogusName: resolveBogusName,
	}

	type queryItems struct {
		query string
		items *pb.RecognizePlacesResponse_Items
	}

	var wg sync.WaitGroup
	resChan := make(chan *queryItems, len(in.GetQueries()))
	for _, query := range in.GetQueries() {
		wg.Add(1)
		go func(query string) {
			defer wg.Done()
			resChan <- &queryItems{query: query, items: pr.detectPlaces(query)}
		}(query)
	}
	wg.Wait()
	close(resChan)

	resp := &pb.RecognizePlacesResponse{
		QueryItems: map[string]*pb.RecognizePlacesResponse_Items{},
	}
	for res := range resChan {
		resp.QueryItems[res.query] = res.items
	}

	return resp, nil
}

func tokenize(query string) []string {
	tokens := []string{}

	// Split by space.
	for _, partBySpace := range strings.Split(query, " ") {
		if partBySpace == "" {
			// This is due to successive spaces, or spaces as prefix or suffix.
			continue
		}

		// Check prefix.
		if string(partBySpace[0]) == "," {
			tokens = append(tokens, ",")
			partBySpace = partBySpace[1:]
			if partBySpace == "" {
				continue
			}
		}

		// Check suffix.
		hasCommaSuffix := false
		if string(partBySpace[len(partBySpace)-1]) == "," {
			hasCommaSuffix = true
			partBySpace = partBySpace[:len(partBySpace)-1]
		}

		// Split by comma. Note |partBySpace| doesn't contain any space.
		partsByComma := strings.Split(partBySpace, ",")
		nPartsByComma := len(partsByComma)
		for idx, partByComma := range partsByComma {
			tokens = append(tokens, partByComma)

			// Add the comma as a token when:
			// 1. Not the last token.
			// 2. The original |partBySpace| has comma as suffix.
			if idx != nPartsByComma-1 || hasCommaSuffix {
				tokens = append(tokens, ",")
			}
		}
	}

	return tokens
}

type placeRecognition struct {
	recogPlaceStore  *files.RecogPlaceStore
	resolveBogusName bool
}

func (p *placeRecognition) detectPlaces(
	query string) *pb.RecognizePlacesResponse_Items {
	tokenSpans := p.replaceTokensWithCandidates(tokenize(query))
	candidates := p.rankAndTrimCandidates(combineContainedIn(tokenSpans), maxPlaceCandidates)
	return formatResponse(query, candidates)
}

func (p *placeRecognition) findPlaceCandidates(
	tokens []string) (int, *pb.RecogPlaces) {
	if len(tokens) == 0 {
		return 0, nil
	}

	// Check if the first token match any abbreviated name.
	// Note: abbreviated names are case-sensitive.
	if places, ok := p.recogPlaceStore.AbbreviatedNameToPlaces[tokens[0]]; ok {
		return 1, places
	}

	key := strings.ToLower(tokens[0])
	places, ok := p.recogPlaceStore.RecogPlaceMap[key]
	if !ok {
		return 0, nil
	}

	numTokens := 1
	// We track the places matched by the span width.  Because we want to
	// always prefer to return the maximally matched span.
	candidatesByNumTokens := make(map[int]*pb.RecogPlaces)
	for _, place := range places.GetPlaces() {
		matchedNameSize := 0
		for _, name := range place.GetNames() {
			nameParts := name.GetParts()
			namePartsSize := len(nameParts)

			// To find a match, tokens cannot be shorter than name parts.
			if len(tokens) < namePartsSize {
				continue
			}

			nameMatched := true
			for i := 0; i < namePartsSize; i++ {
				if nameParts[i] != strings.ToLower(tokens[i]) {
					nameMatched = false
					break
				}
			}

			if nameMatched && matchedNameSize < namePartsSize {
				// Try to match the longest possible name.
				// For example, "New York City" should match 3 tokens instead of 2 tokens.
				matchedNameSize = namePartsSize
			}
		}
		if matchedNameSize == 0 { // This place is not matched.
			continue
		}

		// Take the max size of tokens for all matched places.
		if numTokens < matchedNameSize {
			numTokens = matchedNameSize
		}
		candidates, ok := candidatesByNumTokens[matchedNameSize]
		if !ok {
			candidatesByNumTokens[matchedNameSize] = &pb.RecogPlaces{
				Places: []*pb.RecogPlace{place},
			}
		} else {
			candidates.Places = append(candidates.Places, place)
		}
	}
	// Return the maximally matched span.
	candidates, ok := candidatesByNumTokens[numTokens]
	if !ok {
		return numTokens, &pb.RecogPlaces{}
	}
	return numTokens, candidates
}

func (p *placeRecognition) replaceTokensWithCandidates(tokens []string) *pb.TokenSpans {
	res := &pb.TokenSpans{}
	for len(tokens) > 0 {
		numTokens, candidates := p.findPlaceCandidates(tokens)
		if numTokens > 0 {
			res.Spans = append(res.Spans, &pb.TokenSpans_Span{
				Tokens: tokens[0:numTokens],
				Places: candidates.GetPlaces(),
			})
			tokens = tokens[numTokens:]
		} else {
			res.Spans = append(res.Spans, &pb.TokenSpans_Span{
				Tokens: tokens[0:1],
			})
			tokens = tokens[1:]
		}
	}
	return res
}

func getNumSpansForContainedIn(spans []*pb.TokenSpans_Span, startIdx int) int {
	size := len(spans)

	// Case: "place1, place2".
	if size > startIdx+2 && len(spans[startIdx+2].GetPlaces()) > 0 {
		nextSpanTokens := spans[startIdx+1].GetTokens()
		if len(nextSpanTokens) == 1 && nextSpanTokens[0] == "," {
			return 3
		}
	}

	// Case: "place1 place2"
	if size > startIdx+1 && len(spans[startIdx+1].GetPlaces()) > 0 {
		return 2
	}

	// Case: no contained in.
	return 0
}

func combineContainedInSingle(
	spans []*pb.TokenSpans_Span, startIdx, numSpans int) *pb.TokenSpans_Span {
	startSpan := spans[startIdx]
	endSpan := spans[startIdx+numSpans-1]

	res := &pb.TokenSpans_Span{Tokens: startSpan.Tokens}
	for i := 1; i < numSpans; i++ {
		res.Tokens = append(res.Tokens, spans[startIdx+i].GetTokens()...)
	}

	// This map is used to collect all the places for the combined span, with dedup.
	dcidToRecogPlaces := map[string]*pb.RecogPlace{}

	for _, p1 := range startSpan.GetPlaces() {
		for _, containingPlace := range p1.GetContainingPlaces() {
			for _, p2 := range endSpan.GetPlaces() {
				if containingPlace == p2.GetDcid() {
					dcidToRecogPlaces[p1.GetDcid()] = p1
				}
			}
		}
	}

	if len(dcidToRecogPlaces) > 0 {
		for _, p := range dcidToRecogPlaces {
			res.Places = append(res.Places, p)
		}
		return res
	}

	return nil
}

func combineContainedIn(tokenSpans *pb.TokenSpans) *pb.TokenSpans {
	spans := tokenSpans.GetSpans()
	i := 0
	res := &pb.TokenSpans{}
	for i < len(spans) {
		tokenSpan := spans[i]
		if len(tokenSpan.GetPlaces()) == 0 {
			i++
			res.Spans = append(res.Spans, tokenSpan)
			continue
		}

		numSpans := getNumSpansForContainedIn(spans, i)
		if numSpans == 0 {
			i++
			res.Spans = append(res.Spans, tokenSpan)
			continue
		}

		collapsedTokenSpan := combineContainedInSingle(spans, i, numSpans)
		if collapsedTokenSpan == nil {
			i++
			res.Spans = append(res.Spans, tokenSpan)
		} else {
			i += numSpans
			res.Spans = append(res.Spans, collapsedTokenSpan)
		}
	}

	return res
}

func (p *placeRecognition) rankAndTrimCandidates(
	tokenSpans *pb.TokenSpans,
	maxPlaceCandidates int) *pb.TokenSpans {
	res := &pb.TokenSpans{}
	for _, span := range tokenSpans.GetSpans() {
		if len(span.GetPlaces()) == 0 {
			res.Spans = append(res.Spans, span)
			continue
		}

		// Deal with bogus name (not followed by an ancestor place).
		spanStr := strings.ToLower(strings.Join(span.Tokens, " "))
		if _, ok := p.recogPlaceStore.BogusPlaceNames[spanStr]; ok && !p.resolveBogusName {
			span.Places = nil
			res.Spans = append(res.Spans, span)
			continue
		}

		// Deal with places that are adjectival and have suffixes.
		if _, ok := p.recogPlaceStore.AdjectivalNamesWithSuffix[spanStr]; ok && len(span.Tokens) > 1 && len(span.GetPlaces()) == 1 {
			// Create a new span with the suffix token
			newSpan := &pb.TokenSpans_Span{}
			newSpan.Tokens = span.Tokens[len(span.Tokens)-1:]

			// Drop the last token from the current span
			span.Tokens = span.Tokens[:len(span.Tokens)-1]

			// Add both to the result.
			res.Spans = append(res.Spans, span)
			res.Spans = append(res.Spans, newSpan)
			continue
		}

		// Rank by descending population.
		sort.SliceStable(span.Places, func(i, j int) bool {
			return span.Places[i].GetPopulation() > span.Places[j].GetPopulation()
		})

		if len(span.GetPlaces()) > maxPlaceCandidates {
			// Firstly, pick all place candidates with index < maxPlaceCandidates.
			filteredPlaces := span.Places[:maxPlaceCandidates]

			// For the rest, only pick the places with
			// population >= minPopulationOverMaxPlaceCandidates.
			for i := maxPlaceCandidates; i < len(span.Places); i++ {
				thisPlace := span.Places[i]
				if thisPlace.Population < minPopulationOverMaxPlaceCandidates {
					break
				}
				filteredPlaces = append(filteredPlaces, thisPlace)
			}

			span.Places = filteredPlaces
		}
		res.Spans = append(res.Spans, span)
	}
	return res
}

// Combine successive non-place tokens.
func formatResponse(
	query string, tokenSpans *pb.TokenSpans) *pb.RecognizePlacesResponse_Items {
	res := &pb.RecognizePlacesResponse_Items{}
	spanParts := []string{}
	for _, tokenSpan := range tokenSpans.GetSpans() {
		span := strings.Join(tokenSpan.GetTokens(), " ")
		if len(tokenSpan.GetPlaces()) > 0 {
			if len(spanParts) > 0 {
				res.Items = append(res.Items, &pb.RecognizePlacesResponse_Item{
					Span: strings.Join(spanParts, " "),
				})
				spanParts = []string{}
			}

			places := []*pb.RecognizePlacesResponse_Place{}
			for _, p := range tokenSpan.GetPlaces() {
				places = append(places, &pb.RecognizePlacesResponse_Place{
					Dcid: p.GetDcid(),
				})
			}

			res.Items = append(res.Items, &pb.RecognizePlacesResponse_Item{
				Span:   span,
				Places: places,
			})
		} else {
			spanParts = append(spanParts, span)
		}
	}

	if len(spanParts) > 0 {
		res.Items = append(res.Items, &pb.RecognizePlacesResponse_Item{
			Span: strings.Join(spanParts, " "),
		})
	}

	return res
}
