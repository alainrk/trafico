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

	// Extract resource names (root fields) instead of operation names
	queries, mutations := g.extractResourceNames(graphqlReq.Query)

	// Set headers
	if len(queries) > 0 {
		req.Header.Set(g.queryHeader, strings.Join(queries, ","))
	}
	if len(mutations) > 0 {
		req.Header.Set(g.mutationHeader, strings.Join(mutations, ","))
	}

	g.next.ServeHTTP(rw, req)
}

// extractResourceNames parses the GraphQL query and extracts root field names (resources)
func (g *GraphQLParser) extractResourceNames(query string) ([]string, []string) {
	var queries []string
	var mutations []string

	// Remove comments
	query = removeComments(query)

	// Parse the query to extract root fields
	queries = g.extractRootFieldsFromOperation(query, "query")
	mutations = g.extractRootFieldsFromOperation(query, "mutation")

	return queries, mutations
}

// extractRootFieldsFromOperation extracts root fields from a specific operation type
func (g *GraphQLParser) extractRootFieldsFromOperation(query string, opType string) []string {
	var fields []string

	// Normalize whitespace
	query = regexp.MustCompile(`\s+`).ReplaceAllString(query, " ")
	query = strings.TrimSpace(query)

	var operationBlocks []string

	if opType == "query" {
		// Handle both named queries and anonymous queries
		operationBlocks = g.findOperationBlocks(query, []string{"query", "anonymous"})
	} else if opType == "mutation" {
		operationBlocks = g.findOperationBlocks(query, []string{"mutation"})
	}

	// Extract root fields from each operation block
	for _, block := range operationBlocks {
		rootFields := g.parseRootFields(block)
		fields = append(fields, rootFields...)
	}

	return fields
}

// findOperationBlocks finds operation blocks of the specified types
func (g *GraphQLParser) findOperationBlocks(query string, opTypes []string) []string {
	var blocks []string

	for _, opType := range opTypes {
		var pattern *regexp.Regexp

		if opType == "anonymous" {
			// Match anonymous queries (starting with {)
			pattern = regexp.MustCompile(`^\s*\{`)
		} else {
			// Match named operations
			pattern = regexp.MustCompile(`(?i)\b` + opType + `\s+\w+[^{]*\{`)
		}

		matches := pattern.FindAllStringIndex(query, -1)
		for _, match := range matches {
			// Find the matching closing brace
			block := g.extractBalancedBlock(query, match[0])
			if block != "" {
				blocks = append(blocks, block)
			}
		}

		// Special case for anonymous queries
		if opType == "anonymous" && len(blocks) == 0 {
			// Check if the entire query is an anonymous query
			if strings.HasPrefix(strings.TrimSpace(query), "{") {
				blocks = append(blocks, query)
			}
		}
	}

	return blocks
}

// extractBalancedBlock extracts a balanced block starting from the given position
func (g *GraphQLParser) extractBalancedBlock(query string, startPos int) string {
	// Find the first opening brace
	bracePos := strings.Index(query[startPos:], "{")
	if bracePos < 0 {
		return ""
	}

	start := startPos + bracePos
	braceCount := 0
	inString := false
	escaped := false

	for i := start; i < len(query); i++ {
		char := query[i]

		if escaped {
			escaped = false
			continue
		}

		if char == '\\' {
			escaped = true
			continue
		}

		if char == '"' {
			inString = !inString
			continue
		}

		if !inString {
			if char == '{' {
				braceCount++
			} else if char == '}' {
				braceCount--
				if braceCount == 0 {
					return query[start+1 : i] // Return content between braces
				}
			}
		}
	}

	return ""
}

// parseRootFields extracts root field names from an operation block
func (g *GraphQLParser) parseRootFields(block string) []string {
	var fields []string

	// Remove nested selections by finding top-level fields only
	lines := strings.Split(block, "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)

		// Skip empty lines, comments, and closing braces
		if line == "" || strings.HasPrefix(line, "#") || line == "}" {
			continue
		}

		// Match field pattern: fieldName or fieldName(args)
		fieldPattern := regexp.MustCompile(`^(\w+)(?:\s*\(.*?\))?\s*[{:]?`)
		matches := fieldPattern.FindStringSubmatch(line)

		if len(matches) >= 2 {
			fieldName := matches[1]

			// Skip GraphQL keywords and directives
			if !isGraphQLKeyword(fieldName) && !strings.HasPrefix(fieldName, "@") {
				// Check if this is a root level field (not nested)
				if !strings.Contains(line, "}") || strings.HasSuffix(strings.TrimSpace(line), "{") {
					fields = append(fields, fieldName)
				}
			}
		}
	}

	// Alternative approach: use regex to find top-level fields
	if len(fields) == 0 {
		// Look for patterns like "fieldName {" or "fieldName(args) {"
		rootFieldPattern := regexp.MustCompile(`(?m)^[\s]*(\w+)(?:\s*\([^)]*\))?\s*\{`)
		matches := rootFieldPattern.FindAllStringSubmatch(block, -1)

		for _, match := range matches {
			if len(match) >= 2 {
				fieldName := match[1]
				if !isGraphQLKeyword(fieldName) {
					fields = append(fields, fieldName)
				}
			}
		}
	}

	// Final fallback: simple field extraction
	if len(fields) == 0 {
		simpleFieldPattern := regexp.MustCompile(`(?m)^\s*(\w+)`)
		matches := simpleFieldPattern.FindAllStringSubmatch(block, -1)

		for _, match := range matches {
			if len(match) >= 2 {
				fieldName := match[1]
				if !isGraphQLKeyword(fieldName) && !strings.HasPrefix(fieldName, "@") {
					fields = append(fields, fieldName)
					break // Take first valid field
				}
			}
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
		"type":         true,
		"input":        true,
		"interface":    true,
		"union":        true,
		"enum":         true,
		"scalar":       true,
		"schema":       true,
		"extend":       true,
		"implements":   true,
		"directive":    true,
	}
	return keywords[strings.ToLower(word)]
}
