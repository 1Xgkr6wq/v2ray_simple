package quic

import (
	"crypto/tls"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/e1732a364fed/v2ray_simple/advLayer"
	"github.com/e1732a364fed/v2ray_simple/netLayer"
	"github.com/e1732a364fed/v2ray_simple/utils"
	"github.com/lucas-clemente/quic-go"
	"github.com/lucas-clemente/quic-go/congestion"
	"go.uber.org/zap"
)

//implements advLayer.MuxClient
type Client struct {
	Creator

	arguments

	knownServerMaxStreamCount int32

	serverAddrStr string

	tlsConf tls.Config

	clientconns  map[[16]byte]*connState
	connMapMutex sync.RWMutex
}

func NewClient(addr *netLayer.Addr, alpnList []string, host string, insecure bool, args arguments) *Client {

	if args.hysteriaMaxByteCount <= 0 {
		args.hysteriaMaxByteCount = Default_hysteriaMaxByteCount
	}

	return &Client{
		serverAddrStr: addr.String(),
		tlsConf: tls.Config{
			InsecureSkipVerify: insecure,
			ServerName:         host,
			NextProtos:         alpnList,
		},
		arguments: args,
	}
}

//trimBadConns removes non-Active sessions, 并试图返回 最佳的可用于新stream的session
func (c *Client) trimBadConns() (bestConn *connState) {
	minSessionNum := 10000
	for id, thisState := range c.clientconns {
		if isActive(thisState) {

			if c.knownServerMaxStreamCount == 0 {
				bestConn = thisState
				return
			} else {
				osc := int(thisState.openedStreamCount)

				if osc < int(c.knownServerMaxStreamCount) {

					if osc < minSessionNum {
						bestConn = thisState
						minSessionNum = osc

					}
				}
			}

		} else {
			thisState.CloseWithError(0, "")
			delete(c.clientconns, id)
		}
	}

	if c.knownServerMaxStreamCount != 0 && minSessionNum >= int(c.knownServerMaxStreamCount) {
		return nil
	}

	return
}

func (c *Client) processWhenFull(previous *connState) {
	if previous != nil && c.knownServerMaxStreamCount == 0 {

		c.knownServerMaxStreamCount = previous.openedStreamCount

		if ce := utils.CanLogDebug("QUIC: knownServerMaxStreamCount"); ce != nil {
			ce.Write(zap.Int32("count", c.knownServerMaxStreamCount))
		}

	}
}

//获取已拨号的连接 / 重新从底层拨号。返回 可作 c.DialSubConn 参数 的值.
func (c *Client) GetCommonConn(_ net.Conn) (any, error) {
	return c.getCommonConn(nil)
}

func (c *Client) getCommonConn(_ net.Conn) (*connState, error) {
	//返回 *sessionState.

	//我们采用预先openStream的策略, 来试出哪些session已经满了, 哪些没满
	// 已知的是, a session满了之后, 要等待 0～45秒 或以上的时间, 才能它才可能腾出空位

	//我们对 each session所打开过的stream进行计数，这样就可以探知 服务端 的 最大stream数设置.

	{

		c.connMapMutex.Lock()
		var theState *connState
		if len(c.clientconns) > 0 {
			theState = c.trimBadConns()
		}
		if len(c.clientconns) > 0 {
			c.connMapMutex.Unlock()
			if theState != nil {
				utils.Debug("quic use old " + strconv.Itoa(int(theState.openedStreamCount)))
				return theState, nil

			}
		} else {
			c.clientconns = make(map[[16]byte]*connState)
			c.connMapMutex.Unlock()
		}
	}

	var conn quic.Connection
	var err error

	if c.early {
		utils.Debug("quic Dialing Early")
		conn, err = quic.DialAddrEarly(c.serverAddrStr, &c.tlsConf, &common_DialConfig)

	} else {

		utils.Debug("quic Dialing Connection")
		conn, err = quic.DialAddr(c.serverAddrStr, &c.tlsConf, &common_DialConfig)

	}

	if err != nil {
		if ce := utils.CanLogErr("QUIC:  dial failed"); ce != nil {
			ce.Write(zap.Error(err))
		}
		return nil, err
	}

	if c.useHysteria {

		if c.hysteria_manual {
			bs := NewBrutalSender_M(congestion.ByteCount(c.hysteriaMaxByteCount))
			conn.SetCongestionControl(bs)

		} else {
			bs := NewBrutalSender(congestion.ByteCount(c.hysteriaMaxByteCount))
			conn.SetCongestionControl(bs)

		}
	}

	id := utils.GenerateUUID()

	var result = &connState{Connection: conn, id: id}
	c.connMapMutex.Lock()
	c.clientconns[id] = result
	c.connMapMutex.Unlock()

	return result, nil
}

func (c *Client) DialSubConn(thing any) (net.Conn, error) {
	theState, ok := thing.(*connState)
	if !ok || theState == nil {
		return nil, utils.ErrNilOrWrongParameter
	}
	return c.dialSubConn(theState)
}

func (c *Client) dialSubConn(theState *connState) (net.Conn, error) {

	stream, err := theState.OpenStream()
	if err != nil {

		if theState.redialing {
			theState.redialing = false
			return nil, err
		}

		const tooManyOpenStreamsStr = "too many open streams"
		eStr := err.Error()

		if eStr == tooManyOpenStreamsStr || strings.Contains(eStr, tooManyOpenStreamsStr) {

			if ce := utils.CanLogDebug("DialSubConn session full, open another one"); ce != nil {
				ce.Write(
					zap.String("reason", eStr),
				)
			}

			c.processWhenFull(theState)

			theState2, err := c.getCommonConn(nil)
			if theState2 == nil {
				//再dial还是nil，也许是暂时性的网络错误, 先退出

				return nil, utils.ErrInErr{ErrDesc: "Quic Redialing failed when full session", ErrDetail: err}
			}

			theState2.redialing = true
			return c.dialSubConn(theState2)
		}

		return nil, err

	}

	theState.redialing = false

	atomic.AddInt32(&theState.openedStreamCount, 1)

	return &StreamConn{Stream: stream, laddr: theState.LocalAddr(), raddr: theState.RemoteAddr(), relatedConnState: theState}, nil
}

func (c *Client) IsEarly() bool {
	return c.early
}
func (c *Client) GetPath() string {
	return ""
}
func (*Client) GetCreator() advLayer.Creator {
	return Creator{}
}
