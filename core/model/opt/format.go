package opt

type DefinitionFormat int8

const (
	UnsupportedFormat DefinitionFormat = -1
	YamlFormat        DefinitionFormat = 0
	JsonFormat        DefinitionFormat = 1
)

// Valid values for definition format
var DefinitionFormatValidValues = []string{"yaml", "json"}

// Parse a definition format from a string
func ParseDefinitionFormat(format string) DefinitionFormat {
	switch format {
	case "yaml":
		return YamlFormat
	case "json":
		return JsonFormat
	default:
		return UnsupportedFormat
	}
}

// File extension for the format
func (d DefinitionFormat) Ext() string {
	switch d {
	case YamlFormat:
		return "yml"
	case JsonFormat:
		return "json"
	default:
		return "unsupported"
	}
}