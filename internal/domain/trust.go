package domain

// TrustRequest represents the payload we expect from Shopify.
// WHY IT'S NECESSARY: Go is strictly typed. It needs a blueprint to map raw JSON text into memory.
type TrustRequest struct {
	// IMPORTANT: Notice how 'PhoneHash' starts with a Capital Letter.
	// In Go, Capitalized fields are "Public" (exported). Lowercase are "Private", we need to do that so that fiber in other package can map these out else the functionality will break.
	// If you make this 'phoneHash', the JSON parser cannot see it and it will fail silently.
	PhoneHash string `json:"phone_hash"`

	// The `json:"..."` part is a Struct Tag.
	// WHY IT'S NECESSARY: It tells Go's JSON parser, "When you see 'session_id' in the raw JSON, put that value into this SessionID field."
	SessionID string `json:"session_id"`
}

// TrustResponse is the blueprint for the JSON we send back.
type TrustResponse struct {
	PhoneHash string `json:"phone_hash"`
	Score     int    `json:"score"`
	Action    string `json:"action"`
}
	type WebhookPayload struct {
		//The variables here need to have there name started from capital letters so that the fiber mapping the json can see it 
		PhoneHash string `json:"phone_hash"`
		Reason    string `json:"reason"`
	}
