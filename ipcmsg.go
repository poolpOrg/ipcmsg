/*
 * Copyright (c) 2021 Gilles Chehade <gilles@poolp.org>
 *
 * Permission to use, copy, modify, and distribute this software for any
 * purpose with or without fee is hereby granted, provided that the above
 * copyright notice and this permission notice appear in all copies.
 *
 * THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
 * WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
 * MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
 * ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
 * WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
 * ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
 * OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
 */

package ipcmsg

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"log"
	"os"
	"syscall"

	"github.com/google/uuid"
)

type Channel struct {
	w        chan IPCMessage
	r        chan IPCMessage
	handlers map[IPCMsgType]func(*Channel, IPCMessage)
}

const IPCMSG_HEADER_SIZE = 31

type IPCMsgType uint32

type IPCMsgHdr struct {
	Id     uuid.UUID
	Type   IPCMsgType
	Size   uint16
	HasFd  uint8
	Peerid uint32
	Pid    uint32
}

type IPCMessage struct {
	Hdr  IPCMsgHdr
	Fd   int
	Data []byte
}

func NewChannel(peerid int, fd int) *Channel {
	channel := &Channel{}
	pid := os.Getpid()

	channel.handlers = make(map[IPCMsgType]func(*Channel, IPCMessage))
	channel.w = make(chan IPCMessage)
	channel.r = make(chan IPCMessage)

	// read message from write channel and send to peer fd
	go func() {
		for msg := range channel.w {
			msg.Hdr.Peerid = uint32(peerid)
			msg.Hdr.Pid = uint32(pid)

			// pack msg header and msg data into output buf
			obuf := make([]byte, 0)

			var packed bytes.Buffer
			if err := binary.Write(&packed, binary.BigEndian, &msg.Hdr); err != nil {
				log.Fatal(err)
			}
			obuf = append(obuf, packed.Bytes()...)
			obuf = append(obuf, msg.Data...)

			// if msg has no FD attached, send as is
			if msg.Hdr.HasFd == 0 {
				err := syscall.Sendmsg(fd, obuf, nil, nil, 0)
				if err != nil {
					log.Fatal(err)
				}
				// annnnnnnd... we're done for this msg
				continue
			}

			// an FD is attached, we need to craft a UnixRights control message
			err := syscall.Sendmsg(fd, obuf, syscall.UnixRights([]int{msg.Fd}...), nil, 0)
			if err != nil {
				log.Fatal(err)
			}

			// close the attached FD
			err = syscall.Close(msg.Fd)
			if err != nil {
				log.Fatal(err)
			}
		}
	}()

	// read message from peer fd and write to read channel
	go func() {
		// oh gosh... the fun begins
		for {

			// a buffer to hold the data
			buf := make([]byte, 64*1024)

			// a cmsgbuf for control message, we only expect 1 fd (4 bytes)
			cmsgbuf := make([]byte, syscall.CmsgSpace(1*4))

			// read a msg, for now only expects blocking IO
			n, _, _, _, err := syscall.Recvmsg(fd, buf, cmsgbuf, 0)
			if err != nil {
				log.Fatal(err)
			}
			buf = buf[:n]

			// sometimes we have an FD, sometimes we don't
			// assume there's a control message and try parsing it,
			// if it fails then we assume there's no FD
			// caller can detect this is IPCMsgHdr.HasFlag is 1 and IpcMsg.Fd == -1
			cmsg := true
			scms, err := syscall.ParseSocketControlMessage(cmsgbuf)
			if err != nil {
				if err != syscall.EINVAL {
					log.Fatal(err)
				}
				cmsg = false
			}

			pfd := -1
			if cmsg {
				// we have a control message ...
				// we're only supposed to have one
				if len(scms) != 1 {
					log.Fatal("received more than one control message")
				}
				fds, err := syscall.ParseUnixRights(&scms[0])
				if err != nil {
					log.Fatal(err)
				}

				// we're only supposed to have one FD
				if len(fds) != 1 {
					log.Fatal("received more than one FD")
				}
				pfd = fds[0]
			}

			// we may have multiple messages crammed in our input buffer
			// process them sequentially, parsing header and extracting data
			for {
				// first, decode a header
				var hdr_bin bytes.Buffer
				var hdr IPCMsgHdr
				hdr_bin.Write(buf[:IPCMSG_HEADER_SIZE])
				err = binary.Read(&hdr_bin, binary.BigEndian, &hdr)
				if err != nil {
					log.Fatal(err)
				}

				// unsure if this can happen, sanity check
				if len(buf) < IPCMSG_HEADER_SIZE+int(hdr.Size) {
					log.Fatal("packet too small")
				}

				// now that we have a header, reset peerid and pid
				// extract the right amount of data from input buffer
				// and if a FD is supposed to be attached, use the one
				// we extracted from control message
				msg := IPCMessage{}
				msg.Hdr = hdr
				msg.Hdr.Peerid = uint32(peerid)
				msg.Hdr.Pid = uint32(pid)
				msg.Data = buf[IPCMSG_HEADER_SIZE : IPCMSG_HEADER_SIZE+int(msg.Hdr.Size)]
				msg.Fd = -1
				if msg.Hdr.HasFd != 0 {
					if pfd == -1 {
						// FD exhaustion on receiving end most-likely
					}
					msg.Fd = pfd
					pfd = -1
				}

				// discard consumed data from input buffer
				buf = buf[IPCMSG_HEADER_SIZE+int(msg.Hdr.Size):]

				// message is ready for caller
				channel.r <- msg

				// input buffer is empty, go back to read loop
				if len(buf) == 0 {
					break
				}

				// not sure if short reads can happen,
				// if so they'll be caught by the earlier log.Fatal()
				// and I'll move the input buffer out of the goroutine
				// into the Channel
			}
		}
	}()

	return channel
}

func (channel *Channel) Dispatch() {
	for msg := range channel.Read() {
		handler, exists := channel.handlers[msg.Hdr.Type]
		if !exists {
			panic("RECEIVED UNEXPECTED MESSAGE")
		}

		handler(channel, msg)

		if msg.Fd != -1 {
			syscall.Close(msg.Fd)
		}
	}
}

func (channel *Channel) Handler(msgtype IPCMsgType, handler func(*Channel, IPCMessage)) {
	channel.handlers[msgtype] = handler
}

func createMessage(msgtype IPCMsgType, data interface{}, fd int) IPCMessage {
	msg := IPCMessage{}
	msg.Hdr = IPCMsgHdr{}
	msg.Hdr.Id, _ = uuid.NewRandom()
	msg.Hdr.Type = msgtype
	msg.Hdr.Size = uint16(len(data.([]byte)))
	if fd == -1 {
		msg.Hdr.HasFd = 0
	} else {
		msg.Hdr.HasFd = 1
	}
	msg.Data = data.([]byte)
	msg.Fd = fd

	return msg
}

func createReply(msg IPCMessage, data interface{}, fd int) IPCMessage {
	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(data); err != nil {
		log.Fatal(err)
	}

	reply := IPCMessage{}
	reply.Hdr = IPCMsgHdr{}
	reply.Hdr.Id = msg.Hdr.Id
	reply.Hdr.Type = msg.Hdr.Type
	reply.Hdr.Size = uint16(len(data.([]byte)))
	if fd == -1 {
		reply.Hdr.HasFd = 0
	} else {
		reply.Hdr.HasFd = 1
	}
	reply.Data = data.([]byte)
	reply.Fd = fd

	return reply
}

func (channel *Channel) Read() chan IPCMessage {
	return channel.r
}

func (channel *Channel) Write(msgtype IPCMsgType, data interface{}, fd int) {
	channel.w <- createMessage(msgtype, data, fd)
}

func (channel *Channel) Reply(msg IPCMessage, data interface{}, fd int) {
	channel.w <- createReply(msg, data, fd)
}
