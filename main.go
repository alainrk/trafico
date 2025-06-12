package trafico

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// Config holds the plugin configuration
type Config struct {
	QueryHeader    string `json:"queryHeader,omitempty"`
	MutationHeader string `json:"mutationHeader,omitempty"`
}

// CreateConfig creates the default plugin configuration
func CreateConfig() *Config {
	return &Config{
		QueryHeader:    "X-GraphQL-Queries",
		MutationHeader: "X-GraphQL-Mutations",
	}
}

// GraphQLParser is the main plugin struct
type GraphQLParser struct {
	next           http.Handler
	name           string
	queryHeader    string
	mutationHeader string
}

// GraphQLRequest represents a GraphQL request
type GraphQLRequest struct {
	Query         string         `json:"query"`
	OperationName string         `json:"operationName,omitempty"`
	Variables     map[string]any `json:"variables,omitempty"`
}

// New creates a new plugin instance
func New(ctx context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	if config.QueryHeader == "" {
		config.QueryHeader = "X-GraphQL-Queries"
	}
	if config.MutationHeader == "" {
		config.MutationHeader = "X-GraphQL-Mutations"
	}

	return &GraphQLParser{
		next:           next,
		name:           name,
		queryHeader:    config.QueryHeader,
		mutationHeader: config.MutationHeader,
	}, nil
}

// ServeHTTP implements the http.Handler interface
func (g *GraphQLParser) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	// Only process POST requests with GraphQL content
	if req.Method != http.MethodPost {
		g.next.ServeHTTP(rw, req)
		return
	}

	// Check Content-Type
	contentType := req.Header.Get("Content-Type")
	if !strings.Contains(contentType, "application/json") && !strings.Contains(contentType, "application/graphql") {
		g.next.ServeHTTP(rw, req)
		return
	}

	// Read body
	body, err := io.ReadAll(req.Body)
	if err != nil {
		g.next.ServeHTTP(rw, req)
		return
	}

	// Restore body for downstream handlers
	req.Body = io.NopCloser(bytes.NewReader(body))

	// Parse GraphQL request
	var graphqlReq GraphQLRequest
	if err := json.Unmarshal(body, &graphqlReq); err != nil {
		// If it's not JSON, try to parse as raw GraphQL
		graphqlReq.Query = string(body)
	}

	// Extract operations
	queries, mutations := g.extractOperations(graphqlReq.Query)

	// Set headers
	if len(queries) > 0 {
		req.Header.Set(g.queryHeader, strings.Join(queries, ","))
	}
	if len(mutations) > 0 {
		req.Header.Set(g.mutationHeader, strings.Join(mutations, ","))
	}

	g.next.ServeHTTP(rw, req)
}

// extractOperations parses the GraphQL query and extracts operation names
func (g *GraphQLParser) extractOperations(query string) ([]string, []string) {
	var queries []string
	var mutations []string

	// Remove comments
	query = removeComments(query)

	// Regular expressions for different operation patterns
	// Named operations
	namedOpRe := regexp.MustCompile(`(?m)^\s*(query|mutation)\s+(\w+)`)
	// Anonymous operations
	anonQueryRe := regexp.MustCompile(`(?m)^\s*\{`)
	anonMutationRe := regexp.MustCompile(`(?m)^\s*mutation\s*\{`)
	// Field selections in anonymous queries
	fieldRe := regexp.MustCompile(`\{\s*(\w+)`)

	// Find named operations
	matches := namedOpRe.FindAllStringSubmatch(query, -1)
	for _, match := range matches {
		if len(match) >= 3 {
			opType := match[1]
			opName := match[2]

			switch opType {
			case "query":
				queries = append(queries, opName)
			case "mutation":
				mutations = append(mutations, opName)
			}
		}
	}

	// Check for anonymous operations if no named operations found
	if len(queries) == 0 && len(mutations) == 0 {
		// Check for anonymous mutation
		if anonMutationRe.MatchString(query) {
			// Extract mutation fields
			fieldMatches := fieldRe.FindAllStringSubmatch(query, -1)
			for _, match := range fieldMatches {
				if len(match) >= 2 && !isGraphQLKeyword(match[1]) {
					mutations = append(mutations, match[1])
					break // Take first field as operation name
				}
			}
		} else if anonQueryRe.MatchString(query) {
			// Anonymous query - extract first field
			fieldMatches := fieldRe.FindAllStringSubmatch(query, -1)
			for _, match := range fieldMatches {
				if len(match) >= 2 && !isGraphQLKeyword(match[1]) {
					queries = append(queries, match[1])
					break // Take first field as operation name
				}
			}
		}
	}

	// If still no operations found, try to extract root fields
	if len(queries) == 0 && len(mutations) == 0 {
		// Look for operation type definitions
		if strings.Contains(query, "mutation") {
			mutations = g.extractRootFields(query, "mutation")
		} else {
			queries = g.extractRootFields(query, "query")
		}
	}

	return queries, mutations
}

// extractRootFields extracts the root fields from a GraphQL operation
func (g *GraphQLParser) extractRootFields(query string, opType string) []string {
	var fields []string

	// Find the start of the operation
	var startIdx int
	if opType == "mutation" {
		idx := strings.Index(query, "mutation")
		if idx >= 0 {
			startIdx = idx + len("mutation")
		}
	} else {
		// For queries, start from the beginning or after "query"
		idx := strings.Index(query, "query")
		if idx >= 0 {
			startIdx = idx + len("query")
		}
	}

	// Find the first opening brace
	braceIdx := strings.Index(query[startIdx:], "{")
	if braceIdx < 0 {
		return fields
	}
	startIdx += braceIdx + 1

	// Extract fields until closing brace or nested selection
	fieldRe := regexp.MustCompile(`^\s*(\w+)`)
	remaining := query[startIdx:]

	// Simple extraction of first-level fields
	lines := strings.Split(remaining, "\n")
	for _, line := range lines {
		matches := fieldRe.FindStringSubmatch(line)
		if len(matches) >= 2 && !isGraphQLKeyword(matches[1]) {
			fields = append(fields, matches[1])
			// Only take the first field for operation name
			break
		}
	}

	return fields
}

// removeComments removes GraphQL comments from the query
func removeComments(query string) string {
	// Remove single-line comments
	re := regexp.MustCompile(`#[^\n]*`)
	return re.ReplaceAllString(query, "")
}

// isGraphQLKeyword checks if a word is a GraphQL keyword
func isGraphQLKeyword(word string) bool {
	keywords := map[string]bool{
		"query":        true,
		"mutation":     true,
		"subscription": true,
		"fragment":     true,
		"on":           true,
		"true":         true,
		"false":        true,
		"null":         true,
	}
	return keywords[strings.ToLower(word)]
}
