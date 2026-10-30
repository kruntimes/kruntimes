package runextensionapi

// ListSessionFilesOptions selects one bounded page of direct workspace
// children. A zero Limit lets the Runtime Server apply its default page size.
type ListSessionFilesOptions struct {
	Directory string
	Limit     int
	PageToken string
}
