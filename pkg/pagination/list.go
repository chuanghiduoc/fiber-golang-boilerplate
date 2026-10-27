package pagination

// List is the cursor-pagination envelope for collection responses. It is a
// generic huma output body: handlers return List[T] and the generated OpenAPI
// schema names the item type.
//
// Object is always "list"; URL is the request path (without the query string);
// Data is never nil (an empty page serialises as []); NextCursor is present
// only when HasMore is true.
type List[T any] struct {
	Object     string `json:"object" doc:"Always \"list\"."`
	URL        string `json:"url" doc:"Collection path this page was read from."`
	Data       []T    `json:"data" doc:"Page items."`
	HasMore    bool   `json:"hasMore" doc:"Whether another page exists."`
	NextCursor string `json:"nextCursor,omitempty" doc:"Opaque cursor for the next page; present when hasMore is true."`
}

// NewList builds a collection envelope. data must be a non-nil slice so the
// JSON body is [] rather than null.
func NewList[T any](url string, data []T, hasMore bool, nextCursor string) List[T] {
	if data == nil {
		data = []T{}
	}
	if !hasMore {
		nextCursor = ""
	}
	return List[T]{
		Object:     "list",
		URL:        url,
		Data:       data,
		HasMore:    hasMore,
		NextCursor: nextCursor,
	}
}
