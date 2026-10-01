package httpapi

import "encoding/json"

// jsonMarshal is a seam so the payload encoder can be swapped or stubbed.
var jsonMarshal = json.Marshal
