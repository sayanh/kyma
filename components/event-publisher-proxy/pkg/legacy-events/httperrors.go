package legacy

// An ErrorResponse represents an error with a status code and an error message
type HTTPErrorResponse struct {
	Code  int    `json:"code"`
	Error string `json:"error"`
}
