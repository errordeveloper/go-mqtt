/*
 * Copyright (c) 2013 IBM Corp.
 *
 * All rights reserved. This program and the accompanying materials
 * are made available under the terms of the Eclipse Public License v1.0
 * which accompanies this distribution, and is available at
 * http://www.eclipse.org/legal/epl-v10.html
 *
 * Contributors:
 *    Seth Hoenig
 *    Allan Stockdill-Mander
 *    Mike Robertson
 */

package mqtt

import (
	"code.google.com/p/go.net/websocket"
	"crypto/tls"
	"net"
	"net/url"
	"time"
)

func openConnection(uri *url.URL, tlsc *tls.Config) (conn net.Conn, err error) {
	switch uri.Scheme {
	case "ws":
		conn, err = websocket.Dial(uri.String(), "mqttv3.1", "ws://localhost")
                if err != nil {
                  return
                }
                conn.(*websocket.Conn).PayloadType = websocket.BinaryFrame
	case "tcp":
		conn, err = net.Dial("tcp", uri.Host)
	case "ssl":
		fallthrough
	case "tls":
		fallthrough
	case "tcps":
		conn, err = tls.Dial("tcp", uri.Host, tlsc)
	}
	return
}

// This function is only used for receiving a connack
// when the connection is first started.
// This prevents receiving incoming data while resume
// is in progress if clean session is false.
func connect(c *MqttClient) {
	var data []byte
	var length, pOffset int

	c.trace_v(NET, "connect started")

	for (length == 0) || (pOffset > length) {
		// Just receive the connack and nothing more.
		incomingBuf := make([]byte, 4)
		c.trace_v(NET, "connect waiting for network data")
		incLength, err := c.conn.Read(incomingBuf)
		c.trace_v(NET, "connect read %d bytes of network data", incLength)
		if err != nil {
			c.trace_e(NET, "connect got error")
			c.errors <- err
		}
		length += incLength
		data = append(data, incomingBuf[0:incLength]...)
		if len(data) > 0 {
			// length should be 4 and pOffset should be 0
			pOffset = packet_offset(data)
		}
	}
	c.trace_v(NET, "data: %v", data[:pOffset])
	msg := decode(data[:pOffset])
	if msg != nil {
		c.trace_v(NET, "connect received inbound message, type %v", msg.msgType())
		c.ibound <- msg
	} else {
		c.trace_e(NET, "connect msg was nil")
	}

	return
}

// actually read incoming messages off the wire
// send Message object into ibound channel
func incoming(c *MqttClient) {

	var data []byte
	var length, pOffset int
	var err error

	c.trace_v(NET, "incoming started")

readdata:
	for {
		for (length == 0) || (pOffset > length) {
			incomingBuf := make([]byte, 2048)
			c.trace_v(NET, "incoming waiting for network data")
			incLength, rerr := c.conn.Read(incomingBuf)
			c.trace_v(NET, "incoming read %d bytes of network data", incLength)
			if rerr != nil {
				err = rerr
				break readdata
			}
			length += incLength
			data = append(data, incomingBuf[0:incLength]...)
			if len(data) > 0 {
				pOffset = packet_offset(data)
			}
		}
		c.trace_v(NET, "data: %v", data[:pOffset])
		msg := decode(data[:pOffset])
		if msg != nil {
			c.trace_v(NET, "incoming received inbound message, type %v", msg.msgType())
			c.ibound <- msg
		} else {
			c.trace_c(NET, "incoming msg was nil")
		}
		length -= pOffset
		data = data[pOffset:]
		if length > pOffset {
			pOffset = packet_offset(data)
		}
	}
	// We received an error on read.
	// If disconnect is in progress, swallow error and return
	select {
	case <-c.stopNet:
		c.trace_v(NET, "incoming stopped")
		return
		// Not trying to disconnect, send the error to the errors channel
	default:
		c.trace_e(NET, "incoming stopped with error")
		c.errors <- err
		return
	}
}

// receive a Message object on obound, and then
// actually send outgoing message to the wire
func outgoing(c *MqttClient) {

	c.trace_v(NET, "outgoing started")

	for {
		c.trace_v(NET, "outgoing waiting for an outbound message")
		select {
		case out := <-c.obound:
			msg := out.m
			msgtype := msg.msgType()
			c.trace_v(NET, "obound got msg to write, type: %d", msgtype)
			if msg.QoS() != QOS_ZERO && msg.MsgId() == 0 {
				msg.setMsgId(c.options.mids.getId())
			}
			if out.r != nil {
				c.receipts.put(msg.MsgId(), out.r)
			}
			msg.setTime()
			persist_obound(c.persist, msg)
			_, err := c.conn.Write(msg.Bytes())
			if err != nil {
				c.trace_e(NET, "outgoing stopped with error")
				c.errors <- err
				return
			}

			if (msg.QoS() == QOS_ZERO) &&
				(msgtype == PUBLISH || msgtype == SUBSCRIBE || msgtype == UNSUBSCRIBE) {
				c.receipts.get(msg.MsgId()) <- Receipt{}
				c.receipts.end(msg.MsgId())
			}
			c.lastContact.update()
			c.trace_v(NET, "obound wrote msg, id: %v", msg.MsgId())
		case msg := <-c.oboundP:
			msgtype := msg.msgType()
			c.trace_v(NET, "obound priority msg to write, type %d", msgtype)
			_, err := c.conn.Write(msg.Bytes())
			if err != nil {
				c.trace_e(NET, "outgoing stopped with error")
				c.errors <- err
				return
			}
			c.lastContact.update()
			if msgtype == DISCONNECT {
				c.trace_v(NET, "outbound wrote disconnect, now closing connection")
				c.conn.Close()
				return
			}
		}
	}
}

// receive Message objects on ibound
// store messages if necessary
// send replies on obound
// delete messages from store if necessary
func alllogic(c *MqttClient) {

	c.trace_v(NET, "logic started")

	for {
		c.trace_v(NET, "logic waiting for msg on ibound")

		select {
		case msg := <-c.ibound:
			c.trace_v(NET, "logic got msg on ibound, type %v", msg.msgType())
			persist_ibound(c.persist, msg)
			switch msg.msgType() {
			case PINGRESP:
				c.trace_v(NET, "received pingresp")
				c.pingOutstanding = false
			case CONNACK:
				c.trace_v(NET, "received connack")
				c.begin <- msg.connRC()
				close(c.begin)
			case SUBACK:
				c.trace_v(NET, "received suback, id: %v", msg.MsgId())
				c.receipts.get(msg.MsgId()) <- Receipt{}
				c.receipts.end(msg.MsgId())
				go c.options.mids.freeId(msg.MsgId())
			case UNSUBACK:
				c.trace_v(NET, "received unsuback, id: %v", msg.MsgId())
				c.receipts.get(msg.MsgId()) <- Receipt{}
				c.receipts.end(msg.MsgId())
				go c.options.mids.freeId(msg.MsgId())
			case PUBLISH:
				c.trace_v(NET, "received publish, msgId: %v", msg.MsgId())
				c.trace_v(NET, "putting msg on onPubChan")
				switch msg.QoS() {
				case QOS_TWO:
					c.options.pubChanTwo <- msg
					c.trace_v(NET, "done putting msg on pubChanTwo")
					pubrecMsg := newPubRecMsg()
					pubrecMsg.setMsgId(msg.MsgId())
					c.trace_v(NET, "putting pubrec msg on obound")
					c.obound <- sendable{pubrecMsg, nil}
					c.trace_v(NET, "done putting pubrec msg on obound")
				case QOS_ONE:
					c.options.pubChanOne <- msg
					c.trace_v(NET, "done putting msg on pubChanOne")
					pubackMsg := newPubAckMsg()
					pubackMsg.setMsgId(msg.MsgId())
					c.trace_v(NET, "putting puback msg on obound")
					c.obound <- sendable{pubackMsg, nil}
					c.trace_v(NET, "done putting puback msg on obound")
				case QOS_ZERO:
					c.options.pubChanZero <- msg
					c.trace_v(NET, "done putting msg on pubChanZero")
				}
			case PUBACK:
				c.trace_v(NET, "received puback, id: %v", msg.MsgId())
				c.receipts.get(msg.MsgId()) <- Receipt{}
				c.receipts.end(msg.MsgId())
				go c.options.mids.freeId(msg.MsgId())
			case PUBREC:
				c.trace_v(NET, "received pubrec, id: %v", msg.MsgId())
				id := msg.MsgId()
				pubrelMsg := newPubRelMsg()
				pubrelMsg.setMsgId(id)
				select {
				case c.obound <- sendable{pubrelMsg, nil}:
				case <-time.After(time.Second):
				}
			case PUBREL:
				c.trace_v(NET, "received pubrel, id: %v", msg.MsgId())
				pubcompMsg := newPubCompMsg()
				pubcompMsg.setMsgId(msg.MsgId())
				select {
				case c.obound <- sendable{pubcompMsg, nil}:
				case <-time.After(time.Second):
				}
			case PUBCOMP:
				c.trace_v(NET, "received pubcomp, id: %v", msg.MsgId())
				c.receipts.get(msg.MsgId()) <- Receipt{}
				c.receipts.end(msg.MsgId())
				go c.options.mids.freeId(msg.MsgId())
			}
		case <-c.stopNet:
			c.trace_w(NET, "logic stopped")
			return
		case err := <-c.errors:
			c.trace_e(NET, "logic got error")
			// clean up go routines
			c.stopPing <- true
			// incoming most likely stopped if outgoing stopped,
			// but let it know to stop anyways.
			c.stopNet <- true
			c.options.stopRouter <- true

			close(c.stopPing)
			close(c.stopNet)
			c.conn.Close()

			// Call onConnectionLost or default error handler
			go c.options.onconnlost(c, err)
			return
		}
	}
}
