package goami2

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func connSrvSess(conn net.Conn, repl []string) {
	go func() {
		buf := make([]byte, 1024)
		_, _ = conn.Write([]byte("Asterisk Call Manager/2.10.4\n"))
		_, _ = conn.Read(buf)
		for _, data := range repl {
			_, _ = conn.Write([]byte(data))

		}
	}()
}

func TestClientLogin(t *testing.T) {
	connClint, connSrv := net.Pipe()
	cl := makeClient(connClint)

	t.Run("timeout read prompt", func(t *testing.T) {
		defer func() { cl.timeout = netTimeout }()
		cl.timeout = time.Nanosecond

		err := cl.login("admin", "pwd")
		assert.ErrorContains(t, err, "i/o timeout")
	})

	t.Run("fail on invalid prompt", func(t *testing.T) {
		go func() {
			_, _ = connSrv.Write([]byte("foo bar"))
		}()
		err := cl.login("admin", "pwd")
		assert.ErrorContains(t, err, "unexpected prompt")
	})

	t.Run("fail on invalid AMI message", func(t *testing.T) {
		connSrvSess(connSrv, []string{"invalid message\r\n\r\n"})
		err := cl.login("admin", "pwd")
		assert.ErrorContains(t, err, "failed to read login response")
	})

	t.Run("fail on response status fail", func(t *testing.T) {
		connSrvSess(connSrv,
			[]string{"Response: Error\r\nMessage: Authentication failed\r\n\r\n"})
		err := cl.login("admin", "pwd")
		assert.ErrorContains(t, err, "Authentication failed")
	})

	t.Run("login successfully", func(t *testing.T) {
		connSrvSess(connSrv,
			[]string{"Response: Success\r\nMessage: Authentication accepted\r\n\r\n"})
		err := cl.login("admin", "pwd")
		assert.Nil(t, err)
	})

	t.Run("write to closed connection", func(t *testing.T) {
		_ = connSrv.Close()
		err := cl.login("admin", "pwd")
		assert.ErrorContains(t, err, "failed setup read timeout")
	})
}

func TestClientClose(t *testing.T) {
	setup := func() *Client {
		connClint, _ := net.Pipe()
		return makeClient(connClint)
	}

	t.Run("close connection and channels", func(t *testing.T) {
		cl := setup()

		assert.NotNil(t, cl.conn)
		assert.NotNil(t, cl.recv)
		assert.NotNil(t, cl.err)

		cl.Close()
		assert.Nil(t, cl.conn)
		assert.True(t, isClosedChan(cl.recv))
		assert.True(t, isClosedChan(cl.err))
	})

	t.Run("not panic on multiple close call", func(t *testing.T) {
		cl := setup()
		cl.Close()
		assert.Nil(t, cl.conn)
		assert.True(t, isClosedChan(cl.recv))
		assert.True(t, isClosedChan(cl.err))
		assert.NotPanics(t, func() { cl.Close() })
	})

	t.Run("not panic on", func(t *testing.T) {
		tests := map[string]func(*Client){
			`close conn`: func(cl *Client) { cl.conn.Close() },
			`close conn and recv chan`: func(cl *Client) {
				cl.conn.Close()
				close(cl.recv)
			},
			`close conn and recv and err chan`: func(cl *Client) {
				cl.conn.Close()
				close(cl.recv)
				close(cl.err)
			},
		}
		for name, init := range tests {
			t.Run(name, func(t *testing.T) {
				cl := setup()
				init(cl)
				assert.NotPanics(t, func() { cl.Close() })
			})
		}
	})
}

func TestClientLoopRead(t *testing.T) {
	setup := func() (net.Conn, net.Conn, *Client) {
		connClint, connSrv := net.Pipe()
		cl := makeClient(connClint)
		return connClint, connSrv, cl
	}

	t.Run("stop on context done", func(t *testing.T) {
		_, _, cl := setup()
		chErr := cl.Err()
		ctx, cancel := context.WithCancel(context.Background())
		go cl.loop(ctx)
		cancel()
		err := <-chErr
		assert.ErrorIs(t, err, ErrEOF)
		cl.Close()
	})

	t.Run("stop on conn read error", func(t *testing.T) {
		conn, _, cl := setup()
		chErr := cl.Err()
		go cl.loop(context.Background())
		_ = conn.Close()
		err := <-chErr
		assert.ErrorIs(t, err, ErrEOF)
		cl.Close()
	})

	t.Run("conn read invalid AMI package", func(t *testing.T) {
		_, connSrv, cl := setup()
		go cl.loop(context.Background())
		_, _ = connSrv.Write([]byte("hello\r\nbye\r\n\r\n"))
		err := <-cl.Err()
		assert.ErrorIs(t, err, ErrAMI)

		// loop is still running
		_, _ = connSrv.Write([]byte("Response: Success\r\nMessage: Access granted\r\n\r\n"))
		msg := <-cl.AllMessages()
		assert.Equal(t, "Success", msg.Field("Response"))
		assert.Equal(t, "Access granted", msg.Field("Message"))
	})
}

func TestClientWriteToConnection(t *testing.T) {
	connClient, connSrv := net.Pipe()
	cl := makeClient(connClient)
	buf := bufio.NewReader(connSrv)

	t.Run("Send method", func(t *testing.T) {
		cl.Send([]byte("Action: CoreStatus\n"))
		s, err := buf.ReadString('\n')
		assert.Nil(t, err)
		assert.Equal(t, "Action: CoreStatus\n", s)
	})

	t.Run("MustSend success", func(t *testing.T) {
		go func() {
			err := cl.MustSend([]byte("must send\n"))
			assert.Nil(t, err)
		}()
		s, err := buf.ReadString('\n')
		assert.Nil(t, err)
		assert.Equal(t, "must send\n", s)
	})

	t.Run("Action success", func(t *testing.T) {
		go func() {
			msg := NewAction("Uptime")
			ok := cl.Action(msg)
			assert.True(t, ok)
		}()
		s, err := buf.ReadString('\n')
		assert.Nil(t, err)
		assert.Equal(t, "Action: Uptime\r\n", s)
	})

	t.Run("MustSend and Action fail", func(t *testing.T) {
		_ = connClient.Close()
		msg := NewAction("Uptime")
		ok := cl.Action(msg)
		assert.False(t, ok)

		err := cl.MustSend([]byte("must send\n"))
		assert.ErrorContains(t, err, "io: read/write on closed")
	})

	t.Run("MustSend returns error when client is closed", func(t *testing.T) {
		conn, _ := net.Pipe()
		client := makeClient(conn)
		client.Close()
		err := client.MustSend([]byte("must send\n"))
		assert.ErrorContains(t, err, "closed connection")
	})

	t.Run("MustSend timeout", func(t *testing.T) {
		conn, _ := net.Pipe()
		client := makeClient(conn)
		client.timeout = 1 * time.Millisecond
		err := client.MustSend([]byte("must send\n"))
		assert.ErrorContains(t, err, "write pipe: i/o timeout")
	})
}

func TestClientLoopStreamRead(t *testing.T) {
	packets := getAmiFixtureCall()
	input := strings.Join(packets, "")

	connClient, connSrv := net.Pipe()
	client := makeClient(connClient)

	go func() {
		_, err := connSrv.Write([]byte(input))
		if err != nil {
			t.Error(err.Error())
		}
	}()

	go client.loop(context.Background())

	for i := 0; i < len(packets); i++ {
		msg := <-client.AllMessages()

		assert.Equal(t, msg.String(), packets[i])
	}
}

func TestIsClosedChan(t *testing.T) {
	foo := make(chan struct{})

	assert.False(t, isClosedChan(foo))

	close(foo)
	assert.True(t, isClosedChan(foo))

	foo = nil
	assert.True(t, isClosedChan(foo))
}
