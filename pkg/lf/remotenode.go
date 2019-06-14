/*
 * LF: Global Fully Replicated Key/Value Store
 * Copyright (C) 2018-2019  ZeroTier, Inc.  https://www.zerotier.com/
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program. If not, see <http://www.gnu.org/licenses/>.
 *
 * --
 *
 * You can be released from the requirements of the license by purchasing
 * a commercial license. Buying such a license is mandatory as soon as you
 * develop commercial closed-source software that incorporates or links
 * directly against ZeroTier software without disclosing the source code
 * of your own application.
 */

package lf

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// APIMaxResponseSize is a sanity limit on the maximum size of a response from the LF HTTP API (can be increased)
const APIMaxResponseSize = 4194304

var httpClient = http.Client{Timeout: time.Second * 30}

func apiRequest(url string, m interface{}) ([]byte, error) {
	var requestBody io.Reader
	requestBody = http.NoBody
	method := "GET"
	if m != nil {
		method = "POST"
		json, err := json.Marshal(m)
		if err != nil {
			return nil, err
		}
		requestBody = bytes.NewReader(json)
	}

	req, err := http.NewRequest(method, url, requestBody)
	if err != nil {
		return nil, err
	}
	req.Header.Add("Accept-Encoding", "gzip")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	bodyReader := resp.Body
	if !resp.Uncompressed && strings.Contains(resp.Header.Get("Content-Encoding"), "gzip") {
		bodyReader, err = gzip.NewReader(bodyReader)
		if err != nil {
			return nil, err
		}
	}
	body, err := ioutil.ReadAll(&io.LimitedReader{R: bodyReader, N: int64(APIMaxResponseSize)})
	if err != nil {
		return nil, err
	}
	bodyReader.Close()

	if resp.StatusCode != http.StatusOK {
		var e ErrAPI
		err = json.Unmarshal(body, &e)
		if err != nil {
			return nil, err
		}
		return nil, e
	}

	return body, nil
}

// RemoteNode is a node reachable over HTTP(S).
type RemoteNode string

// NewRemoteNode constructs a new remote node from a URL in string format.
func NewRemoteNode(urlStr string) (RemoteNode, error) {
	u, err := url.Parse(urlStr)
	if err != nil {
		return "", err
	}
	upstr := u.String()
	if len(upstr) == 0 {
		return "", ErrInvalidParameter
	}
	for upstr[len(upstr)-1] == '/' {
		upstr = upstr[0 : len(upstr)-1]
		if len(upstr) == 0 {
			return "", ErrInvalidParameter
		}
	}
	return RemoteNode(upstr), nil
}

// AddRecord submits this record for addition to the data store.
func (rn RemoteNode) AddRecord(rec *Record) error {
	resp, err := httpClient.Post(string(rn)+"/post", "application/octet-stream", bytes.NewReader(rec.Bytes()))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		bodyReader := resp.Body
		if !resp.Uncompressed && strings.Contains(resp.Header.Get("Content-Encoding"), "gzip") {
			bodyReader, err = gzip.NewReader(bodyReader)
			if err != nil {
				return err
			}
		}
		body, err := ioutil.ReadAll(&io.LimitedReader{R: bodyReader, N: int64(APIMaxResponseSize)})
		if err != nil {
			return err
		}
		bodyReader.Close()

		var e ErrAPI
		err = json.Unmarshal(body, &e)
		if err != nil {
			return err
		}
		return e
	}
	return nil
}

// GetRecord looks up a record by its exact hash.
func (rn RemoteNode) GetRecord(hash []byte) (*Record, error) {
	if len(hash) == 32 {
		return nil, ErrInvalidParameter
	}
	body, err := apiRequest(string(rn)+"/record/raw/="+Base62Encode(hash), nil)
	if err != nil {
		return nil, err
	}
	return NewRecordFromBytes(body)
}

// GenesisParameters returns this network's current global parameters.
func (rn RemoteNode) GenesisParameters() (*GenesisParameters, error) {
	ns, err := rn.NodeStatus()
	if err != nil {
		return nil, err
	}
	return &ns.GenesisParameters, nil
}

// NodeStatus gets the status of the remote node.
func (rn RemoteNode) NodeStatus() (*NodeStatus, error) {
	body, err := apiRequest(string(rn)+"/status", nil)
	if err != nil {
		return nil, err
	}
	var ns NodeStatus
	err = json.Unmarshal(body, &ns)
	if err != nil {
		return nil, err
	}
	return &ns, nil
}

// OwnerStatus gets the status of an owner and also returns some links that can be used to make a new record.
func (rn RemoteNode) OwnerStatus(ownerPublic OwnerPublic) (*OwnerStatus, error) {
	if len(ownerPublic) == 0 {
		return nil, ErrInvalidParameter
	}
	body, err := apiRequest(string(rn)+"/owner/"+ownerPublic.String(), nil)
	if err != nil {
		return nil, err
	}
	var os OwnerStatus
	err = json.Unmarshal(body, &os)
	if err != nil {
		return nil, err
	}
	return &os, nil
}

// Links returns up to count links or the network's min links per record if count is <= 0.
func (rn RemoteNode) Links(count int) ([][32]byte, int64, error) {
	u := string(rn) + "/links"
	if count > 0 {
		u = u + "?count=" + strconv.FormatUint(uint64(count), 10)
	}
	resp, err := httpClient.Get(u)
	if err != nil {
		return nil, -1, err
	}
	if resp.StatusCode == 200 {
		body, err := ioutil.ReadAll(&io.LimitedReader{R: resp.Body, N: APIMaxResponseSize})
		resp.Body.Close()
		if err != nil {
			return nil, -1, err
		}
		var l [][32]byte
		for i := 0; (i + 32) <= len(body); i += 32 {
			var h [32]byte
			copy(h[:], body[i:i+32])
			l = append(l, h)
		}
		tstr := resp.Header.Get("X-LF-Time")
		if len(tstr) > 0 {
			ts, _ := strconv.ParseInt(tstr, 10, 64)
			return l, ts, nil
		}
		return l, -1, nil
	}
	return nil, -1, ErrAPI{Code: resp.StatusCode}
}

// ExecuteQuery executes a query against this remote node.
func (rn RemoteNode) ExecuteQuery(q *Query) (QueryResults, error) {
	body, err := apiRequest(string(rn)+"/query", q)
	if err != nil {
		return nil, err
	}
	var qr QueryResults
	err = json.Unmarshal(body, &qr)
	if err != nil {
		return nil, err
	}
	return qr, nil
}

// Connect instructs this node to initiate a remote connection
func (rn RemoteNode) Connect(ip net.IP, port int, identity []byte) error {
	_, err := apiRequest(string(rn)+"/connect", &Peer{
		IP:       ip,
		Port:     port,
		Identity: identity,
	})
	return err
}

// IsLocal always returns false for RemoteNode.
func (rn RemoteNode) IsLocal() bool { return false }