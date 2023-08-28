package geecache

import (
	"fmt"
	"geecache/geecache/consistenthash"
	pb "geecache/geecachepb"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"
)

// const defaultBasePath = "/_geecache/"

// // HTTPPool implements PeerPicker for a pool of HTTP peers.
// // 承载节点间 HTTP 通信的核心数据结构（包括服务端和客户端）
//
//	type HTTPPool struct {
//		// this peer's base URL, e.g. "https://example.net:8000"
//		self     string //记录自己的地址，包括主机名/IP 和端口
//		basePath string //作为节点间通讯地址的前缀，默认是 /_geecache/
//	}
const (
	defaultBasePath = "/_geecache/"
	defaultReplicas = 50
)

// HTTPPool implements PeerPicker for a pool of HTTP peers.
type HTTPPool struct {
	// this peer's base URL, e.g. "https://example.net:8000"
	self        string
	basePath    string
	mu          sync.Mutex             // guards peers and httpGetters
	peers       *consistenthash.Map    //类型是一致性哈希算法的 Map，用来根据具体的 key 选择节点
	httpGetters map[string]*httpGetter // keyed by e.g. "http://10.0.0.2:8008"
	//映射远程节点与对应的 httpGetter 每一个远程节点对应一个 httpGetter，因为 httpGetter 与远程节点的地址 baseURL 有关
}

// NewHTTPPool initializes an HTTP pool of peers.
func NewHTTPPool(self string) *HTTPPool {
	return &HTTPPool{
		self:     self,
		basePath: defaultBasePath,
	}
}

// Log info with server name
// 带有服务器名称的日志信息
func (p *HTTPPool) Log(format string, v ...interface{}) {
	log.Printf("[Server %s] %s", p.self, fmt.Sprintf(format, v...))
}

// Set updates the pool's list of peers.
// Set() 方法实例化了一致性哈希算法，并且添加了传入的节点 并为每一个节点创建了一个 HTTP 客户端 httpGetter
func (p *HTTPPool) Set(peers ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.peers = consistenthash.New(defaultReplicas, nil)
	p.peers.Add(peers...)
	p.httpGetters = make(map[string]*httpGetter, len(peers))
	for _, peer := range peers {
		p.httpGetters[peer] = &httpGetter{baseURL: peer + p.basePath}
	}
}

// ServeHTTP handle all http requests
func (p *HTTPPool) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, p.basePath) { // 判断访问路径的前缀是否是 basePath，不是返回错误
		panic("HTTPPool serving unexpected path: " + r.URL.Path) //HTTPPool提供意外路径
	}
	p.Log("%s %s", r.Method, r.URL.Path)
	//将给定的字符串拆分为由分隔符分隔的子字符串。此函数返回这些分隔符之间所有子字符串的切片
	//n大于零（n>0）：最多返回n个子字符串，最后一个字符串是未分割的剩余部分。
	//约定访问路径格式为 /<basepath>/<groupname>/<key>
	parts := strings.SplitN(r.URL.Path[len(p.basePath):], "/", 2)
	if len(parts) != 2 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	groupName := parts[0]
	key := parts[1]

	group := GetGroup(groupName) //通过 groupname 得到 group 实例
	if group == nil {
		http.Error(w, "no such group: "+groupName, http.StatusNotFound) // 请求的资源（网页等）不存在
		return
	}

	view, err := group.Get(key) //获取缓存数据
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError) //内部服务器错误
		return
	}

	// Write the value to the response body as a proto message.
	body, err := proto.Marshal(&pb.Response{Value: view.ByteSlice()})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	//application/octet-stream 通常在下载文件场景中使用，它会告知浏览器写入的内容是一个字节流，浏览器处理字节流的默认方式就是下载

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(body)
}

// ======为 HTTPPool 实现客户端的功能=====//

// 创建具体的 HTTP 客户端类 httpGetter，实现 PeerGetter 接口
type httpGetter struct {
	baseURL string //将要访问的远程节点的地址，例如 http://example.com/_geecache/
}

// 使用 http.Get() 方式获取返回值，并转换为 []bytes 类型
func (h *httpGetter) Get(in *pb.Request, out *pb.Response) error {
	u := fmt.Sprintf(
		"%v%v/%v",
		h.baseURL,
		url.QueryEscape(in.GetGroup()), //QueryEscape函数对group进行转码使之可以安全的用在URL查询里
		url.QueryEscape(in.GetKey()),
	)
	res, err := http.Get(u) //从指定的资源请求数据
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned: %v", res.Status) //服务器返回状态码
	}

	bytes, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return fmt.Errorf("reading response body: %v", err) //阅读响应正文
	}

	if err = proto.Unmarshal(bytes, out); err != nil {
		return fmt.Errorf("decoding response body: %v", err)
	}

	return nil
}

var _ PeerGetter = (*httpGetter)(nil) //确保这个类型实现了这个接口 如果没有实现会报错的

// PickPeer picks a peer according to key
// PickerPeer() 包装了一致性哈希算法的 Get() 方法，根据具体的 key，选择节点，返回节点对应的 HTTP 客户端。
func (p *HTTPPool) PickPeer(key string) (PeerGetter, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if peer := p.peers.Get(key); peer != "" && peer != p.self {
		p.Log("Pick peer %s", peer)
		return p.httpGetters[peer], true
	}
	return nil, false
}

var _ PeerPicker = (*HTTPPool)(nil)
