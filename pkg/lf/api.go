package lf

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/vmihailenco/msgpack"
)

// APIVersion is the version of the current implementation
const APIVersion = uint64(1)

// APIMaxResults is the global maximum number of results allowed in any query.
const APIMaxResults = 128

// APIPeer contains information about a connected peer for APIStatus.
type APIPeer struct {
	ProtoMessagePeer
	TotalBytesSent     uint64 `msgpack:"TBS"` // Total bytes sent to this peer
	TotalBytesReceived uint64 `msgpack:"TBR"` // Total bytes received from this peer
	Latency            int    `msgpack:"L"`   // Latency in millisconds or -1 if not known
}

// APIStatus contains status information about this node and the network it belongs to.
type APIStatus struct {
	Software       string    `msgpack:"S"`    // Software implementation name
	Version        [4]int    `msgpack:"V"`    // Version of software
	MinAPIVersion  uint64    `msgpack:"MinA"` // Minimum API version supported
	MaxAPIVersion  uint64    `msgpack:"MaxA"` // Maximum API version supported
	Uptime         uint64    `msgpack:"U"`    // Uptime in milliseconds since epoch
	ConnectedPeers []APIPeer `msgpack:"CP"`   // Connected peer nodes (if revealed by node)
	DBRecordCount  uint64    `msgpack:"DBRC"` // Number of records in database
	DBSize         uint64    `msgpack:"DBS"`  // Total size of records in database in bytes
}

// APIPut (/p) is used to submit a new record revision to the global LF key/value store.
// If Data is non-nil/empty it must contain a valid and fully signed and paid for record. If
// this is present all other fields are ignored. If Data is not present the other fields contain
// the values that are required for the node to locally build and sign the record. Nodes only
// allow this (for both security and DOS reasons) from authorized clients. Use the Proxy to do
// this from other places. The Proxy accepts requests to localhosts, passed through queries,
// but intercepts puts and builds records locally and then submits them in Data to a full node.
type APIPut struct {
	Data            []byte    `msgpack:"D,omitempty" json:",omitempty"`   // Fully encoded record data, overrides other fields if present
	Key             []byte    `msgpack:"K,omitempty" json:",omitempty"`   // Plain text key
	Value           []byte    `msgpack:"V,omitempty" json:",omitempty"`   // Plain text value
	OwnerPrivateKey []byte    `msgpack:"OPK,omitempty" json:",omitempty"` // Owner private key to sign record
	Selectors       [2][]byte `msgpack:"S,omitempty" json:",omitempty"`   // Selectors
	PlainTextValue  bool      `msgpack:"PTV"`                             // If true, do not encrypt value in record
}

// APIGet (/g) gets records by search keys.
type APIGet struct {
	Key         []byte    `msgpack:"K,omitempty" json:",omitempty"`    // Plain text key (overrides ID)
	ID          []byte    `msgpack:"ID,omitempty" json:",omitempty"`   // ID (32 bytes) (ignored if Key is given)
	Owner       []byte    `msgpack:"O,omitempty" json:",omitempty"`    // Owner (32 bytes)
	SelectorIDs [2][]byte `msgpack:"SIDs,omitempty" json:",omitempty"` // Selector IDs (32 bytes each)
	MaxResults  uint      `msgpack:"MR,omitempty" json:",omitempty"`   // Maximum total results or 0 for unlimited
}

// APIRecordDetail is sent (in an array) in response to APIGet.
type APIRecordDetail struct {
	Record Record   `msgpack:"R"`                             // Fully unpacked record
	Key    []byte   `msgpack:"K,omitempty" json:",omitempty"` // Plain-text key if supplied in query, otherwise omitted
	Value  []byte   `msgpack:"V,omitempty" json:",omitempty"` // Plain-text value if plain-text key was supplied with query, otherwise omitted
	Weight [16]byte `msgpack:"W,omitempty" json:",omitempty"` // Weight of record as a 128-bit big-endian number
}

// APIRequestLinks is a request for links to include in a new record.
type APIRequestLinks struct {
	Count uint `msgpack:"C"` // Desired number of links
}

// APILinks is a set of links returned by APIRequestLinks
type APILinks struct {
	Links []byte `msgpack:"L"` // Array of links (size is always a multiple of 32 bytes, link count is size / 32)
}

// APIError indicates an error and is returned with non-200 responses.
type APIError struct {
	Code    int    `msgpack:"C"` // Positive error codes simply mirror HTTP response codes, while negative ones are LF-specific
	Message string `msgpack:"M"` // Message indicating the reason for the error
}

func apiMakePeerArray(n *Node) []APIPeer {
	n.hostsLock.RLock()
	defer n.hostsLock.RUnlock()
	r := make([]APIPeer, 0, len(n.hosts))
	for i := range n.hosts {
		if n.hosts[i].Connected() {
			var at byte
			if len(n.hosts[i].RemoteAddress.IP) == 16 {
				at = 6
			} else {
				at = 4
			}
			r = append(r, APIPeer{
				ProtoMessagePeer: ProtoMessagePeer{
					Protocol:    ProtoTypeLFRawUDP,
					AddressType: at,
					IP:          n.hosts[i].RemoteAddress.IP,
					Port:        uint16(n.hosts[i].RemoteAddress.Port),
				},
				TotalBytesSent:     n.hosts[i].TotalBytesSent,
				TotalBytesReceived: n.hosts[i].TotalBytesReceived,
				Latency:            n.hosts[i].Latency,
			})
		}
	}
	return r
}

func apiSendJSON(out http.ResponseWriter, req *http.Request, httpStatusCode int, obj interface{}) error {
	out.Header().Set("Pragma", "no-cache")
	out.Header().Set("Cache-Control", "no-cache")

	// If the client elects that it accepts msgpack, send that instead since it's faster and smaller.
	accept, haveAccept := req.Header["Accept"]
	if haveAccept {
		for i := range accept {
			asp := strings.FieldsFunc(accept[i], func(r rune) bool {
				return (r == ',' || r == ';' || r == ' ' || r == '\t')
			})
			for j := range asp {
				asp[j] = strings.TrimSpace(asp[j])
				if strings.Contains(asp[j], "msgpack") {
					out.Header().Set("Content-Type", asp[j])
					out.WriteHeader(httpStatusCode)
					if req.Method == http.MethodHead {
						return nil
					}
					return msgpack.NewEncoder(out).Encode(obj)
				}
			}
		}
	}

	out.Header().Set("Content-Type", "application/json")
	out.WriteHeader(httpStatusCode)
	if req.Method == http.MethodHead {
		return nil
	}
	return json.NewEncoder(out).Encode(obj)
}

func apiReadJSON(out http.ResponseWriter, req *http.Request, dest interface{}) (err error) {
	// The same msgpack support is present for incoming requests and messages if set by content-type. Otherwise assume JSON.
	decodedMsgpack := false
	ct, haveCT := req.Header["Content-Type"]
	if haveCT {
		for i := range ct {
			if strings.Contains(ct[i], "msgpack") {
				err = msgpack.NewDecoder(req.Body).Decode(&dest)
				decodedMsgpack = true
			}
		}
	}
	if !decodedMsgpack {
		err = json.NewDecoder(req.Body).Decode(&dest)
	}
	if err != nil {
		apiSendJSON(out, req, http.StatusBadRequest, &APIError{Code: http.StatusBadRequest, Message: "invalid or malformed payload"})
	}
	return err
}

func apiIsTrusted(n *Node, req *http.Request) bool {
	// TODO: use HTTP auth or configurable host list
	return strings.HasPrefix(req.RemoteAddr, "127.0.0.1") || strings.HasPrefix(req.RemoteAddr, "::1") || strings.HasPrefix(req.RemoteAddr, "[::1]")
}

func apiCreateHTTPServeMux(n *Node) *http.ServeMux {
	smux := http.NewServeMux()

	// Get best value by record key. The key may be /k/<key>.ext or /k/_<base64url>.ext for a base64url encoded
	// key. The extension determins what type is returned. A json or msgpack extension returns an APIRecord object.
	// The following extensions return the value with the appropriate content type: html, js, png, gif, jpg, xml,
	// css, and txt. Other extensions will return 404. No extension returns value with type application/octet-stream.
	smux.HandleFunc("/k/", func(out http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet || req.Method == http.MethodHead {
		} else {
			apiSendJSON(out, req, http.StatusMethodNotAllowed, &APIError{Code: http.StatusMethodNotAllowed, Message: req.Method + " not supported for this path"})
		}
	})

	// Post a record, takes APIPut payload or just a raw record.
	smux.HandleFunc("/p", func(out http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost || req.Method == http.MethodPut {
			// Handle submission of raw records in raw record format with no enclosing object.
			ct, haveCT := req.Header["Content-Type"]
			if haveCT {
				for i := range ct {
					if strings.Contains(ct[i], "application/x-lf-record") {
						var rdata [RecordMaxSize]byte
						n, _ := io.ReadFull(req.Body, rdata[:])
						if n > RecordMinSize {
						} else {
							apiSendJSON(out, req, http.StatusBadRequest, &APIError{Code: http.StatusBadRequest, Message: "invalid or malformed payload"})
							return
						}
					}
				}
			}

			var put APIPut
			if apiReadJSON(out, req, &put) == nil {
				if len(put.Data) > 0 {
				} else if apiIsTrusted(n, req) {
				} else {
					apiSendJSON(out, req, http.StatusForbidden, &APIError{Code: http.StatusForbidden, Message: "node will only build records locally if submitted from authorized hosts"})
				}
			}
		} else {
			apiSendJSON(out, req, http.StatusMethodNotAllowed, &APIError{Code: http.StatusMethodNotAllowed, Message: req.Method + " not supported for this path"})
		}
	})

	// Get record, takes APIGet payload for parameters. (Ironically /g must be gotten with PUT or POST!)
	smux.HandleFunc("/g", func(out http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost || req.Method == http.MethodPut {
		} else {
			apiSendJSON(out, req, http.StatusMethodNotAllowed, &APIError{Code: http.StatusMethodNotAllowed, Message: req.Method + " not supported for this path"})
		}
	})

	// Raw record request, payload is raw binary 32-byte hashes rather than a JSON message.
	// The node is free to send other records in response as well, and the receiver should import
	// records in the order in which they are sent. Results are sent in binary raw form with
	// each record prefixed by a 16-bit (big-endian) record size.
	smux.HandleFunc("/r", func(out http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost || req.Method == http.MethodPut {
		} else {
			apiSendJSON(out, req, http.StatusMethodNotAllowed, &APIError{Code: http.StatusMethodNotAllowed, Message: req.Method + " not supported for this path"})
		}
	})

	smux.HandleFunc("/peers", func(out http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet || req.Method == http.MethodHead {
			apiSendJSON(out, req, http.StatusOK, apiMakePeerArray(n))
		} else {
			apiSendJSON(out, req, http.StatusMethodNotAllowed, &APIError{Code: http.StatusMethodNotAllowed, Message: req.Method + " not supported for this path"})
		}
	})

	smux.HandleFunc("/connect", func(out http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost || req.Method == http.MethodPut {
			if apiIsTrusted(n, req) {
			} else {
				apiSendJSON(out, req, http.StatusForbidden, &APIError{Code: http.StatusForbidden, Message: "peers may only be submitted by trusted hosts"})
			}
		} else {
			apiSendJSON(out, req, http.StatusMethodNotAllowed, &APIError{Code: http.StatusMethodNotAllowed, Message: req.Method + " not supported for this path"})
		}
	})

	smux.HandleFunc("/status", func(out http.ResponseWriter, req *http.Request) {
		rc, ds := n.db.stats()
		var s APIStatus
		s.Software = SoftwareName
		s.Version[0] = VersionMajor
		s.Version[1] = VersionMinor
		s.Version[2] = VersionRevision
		s.Version[3] = VersionBuild
		s.MinAPIVersion = APIVersion
		s.MaxAPIVersion = APIVersion
		s.Uptime = n.startTime
		s.ConnectedPeers = apiMakePeerArray(n)
		s.DBRecordCount = rc
		s.DBSize = ds
		apiSendJSON(out, req, http.StatusOK, &s)
	})

	smux.HandleFunc("/", func(out http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet || req.Method == http.MethodHead {
			if req.URL.Path == "/" {
			} else {
				apiSendJSON(out, req, http.StatusNotFound, &APIError{Code: http.StatusNotFound, Message: req.URL.Path + " is not a valid path"})
			}
		} else {
			apiSendJSON(out, req, http.StatusMethodNotAllowed, &APIError{Code: http.StatusMethodNotAllowed, Message: req.Method + " not supported for this path"})
		}
	})

	return smux
}