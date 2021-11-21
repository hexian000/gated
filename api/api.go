package api

const (
	WebCluster = "/cluster"
	WebStatus  = "/status"
)

// func (s *Server) makeRequset(path string, data interface{}) (*http.Request, error) {
// 	host := s.GetAPIHost()
// 	fn, err := url.Parse(fmt.Sprintf("http://%s%s", host, path))
// 	if err != nil {
// 		return nil, err
// 	}
// 	b, err := json.Marshal(data)
// 	if err != nil {
// 		return nil, err
// 	}
// 	req := &http.Request{
// 		Method:     http.MethodPost,
// 		URL:        fn,
// 		Host:       host,
// 		ProtoMajor: 1,
// 		ProtoMinor: 1,
// 		Header:     make(http.Header),
// 		Body:       io.NopCloser(bytes.NewBuffer(b)),
// 	}
// 	req.Header.Set("Content-Type", "application/json; charset=utf-8")
// 	return req, nil
// }

// func (s *Server) Call(peer, path string, data interface{}) error {
// 	req, err := s.makeRequset(path, data)
// 	if err != nil {
// 		return err
// 	}
// 	client := s.apiClient(peer)
// 	resp, err := client.Do(req)
// 	if err != nil {
// 		return err
// 	}
// 	defer resp.Body.Close()
// 	if resp.StatusCode != http.StatusAccepted {
// 		msg, err := ioutil.ReadAll(resp.Body)
// 		if err != nil {
// 			return err
// 		}
// 		slog.Errorf("call %s %s: [%d %s] %s", peer, path, resp.StatusCode, resp.Status, string(msg))
// 	}
// 	return nil
// }
