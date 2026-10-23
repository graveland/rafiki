// SPDX-License-Identifier: Apache-2.0
//! Connect streaming envelopes: a flags byte, a 4-byte big-endian length, then
//! that many payload bytes.
//!
//! Envelope boundaries have nothing to do with body-frame boundaries: one HTTP
//! chunk may hold half a header, two payloads, or the tail of one envelope and
//! the head of the next. [`EnvelopeDecoder`] buffers and slices exactly, and
//! treats a body that ends mid-envelope as an error rather than a short read.

use bytes::{Buf, Bytes, BytesMut};

use crate::{Code, Error};

/// Flags bit 0: the payload is compressed. This client never compresses and
/// never agrees to decompress.
pub(crate) const FLAG_COMPRESSED: u8 = 0x01;

/// Flags bit 1: this envelope is the end-of-stream trailer.
pub(crate) const FLAG_END: u8 = 0x02;

/// flags byte + 4-byte big-endian length.
const HEADER_LEN: usize = 5;

/// The largest message, envelope payload or response body this client will
/// buffer. A length field is trusted only up to here, so a corrupt or hostile
/// one is refused instead of growing the buffer toward 4 GiB. Ingested images
/// are resized to at most 1.15MP, so a real event is far below this.
pub(crate) const MAX_PAYLOAD: usize = 64 << 20;

/// One envelope as it arrived.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct Envelope {
    pub(crate) flags: u8,
    pub(crate) payload: Bytes,
}

/// Wrap one payload in the envelope a streaming REQUEST body is: exactly one,
/// always uncompressed.
pub(crate) fn envelop(payload: &[u8]) -> Bytes {
    let mut out = BytesMut::with_capacity(HEADER_LEN + payload.len());
    out.extend_from_slice(&[0u8]);
    out.extend_from_slice(&(payload.len() as u32).to_be_bytes());
    out.extend_from_slice(payload);
    out.freeze()
}

/// A buffering envelope reader.
#[derive(Debug, Default)]
pub(crate) struct EnvelopeDecoder {
    buf: BytesMut,
    eof: bool,
}

impl EnvelopeDecoder {
    pub(crate) fn new() -> EnvelopeDecoder {
        EnvelopeDecoder {
            buf: BytesMut::new(),
            eof: false,
        }
    }

    /// Append bytes from the next body frame. Frames may split or pack
    /// envelopes arbitrarily.
    pub(crate) fn feed(&mut self, frame: &[u8]) {
        self.buf.extend_from_slice(frame);
    }

    /// The body is over: whatever is buffered is all there will ever be.
    pub(crate) fn finish(&mut self) {
        self.eof = true;
    }

    /// The next complete envelope, `None` if more bytes are needed. At EOF a
    /// partial envelope is [`Code::Unavailable`] — the connection died, it did
    /// not say goodbye.
    ///
    /// A boundary EOF with nothing buffered is also `Ok(None)`: the decoder
    /// only says "no partial envelope", not that the stream is over. Whether a
    /// boundary EOF is a clean close is the stream layer's call, and it is one
    /// only after a flags-`0x02` end-of-stream envelope.
    pub(crate) fn next(&mut self) -> Result<Option<Envelope>, Error> {
        if self.buf.len() < HEADER_LEN {
            if self.eof && !self.buf.is_empty() {
                return Err(closed_mid_envelope());
            }
            return Ok(None);
        }
        let flags = self.buf[0];
        let length = u32::from_be_bytes([self.buf[1], self.buf[2], self.buf[3], self.buf[4]]);
        let length = length as usize;
        if length > MAX_PAYLOAD {
            return Err(Error::new(
                Code::ResourceExhausted,
                format!("stream envelope of {length} bytes exceeds {MAX_PAYLOAD}"),
            ));
        }
        if self.buf.len() < HEADER_LEN + length {
            if self.eof {
                return Err(closed_mid_envelope());
            }
            return Ok(None);
        }
        self.buf.advance(HEADER_LEN);
        let payload = self.buf.split_to(length).freeze();
        Ok(Some(Envelope { flags, payload }))
    }
}

fn closed_mid_envelope() -> Error {
    Error::new(Code::Unavailable, "stream closed mid-envelope")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn one_envelope_split_over_three_frames() {
        let mut decoder = EnvelopeDecoder::new();
        let wire = envelop(b"hello world");
        for piece in wire.chunks(4).take(3) {
            decoder.feed(piece);
            assert!(
                decoder.next().expect("no error").is_none(),
                "a partial envelope is not data"
            );
        }
        decoder.feed(&wire[12..]);
        let envelope = decoder.next().expect("no error").expect("one envelope");
        assert_eq!(envelope.flags, 0);
        assert_eq!(envelope.payload, Bytes::from_static(b"hello world"));
        assert!(decoder.next().expect("no error").is_none());
    }

    #[test]
    fn two_envelopes_in_one_frame() {
        let mut wire = BytesMut::from(&envelop(b"first")[..]);
        wire.extend_from_slice(&envelop(b"second"));
        let mut decoder = EnvelopeDecoder::new();
        decoder.feed(&wire);
        let first = decoder.next().expect("no error").expect("first");
        assert_eq!(first.payload, Bytes::from_static(b"first"));
        let second = decoder.next().expect("no error").expect("second");
        assert_eq!(second.payload, Bytes::from_static(b"second"));
        assert!(decoder.next().expect("no error").is_none());
        assert!(decoder.buf.is_empty());
    }

    #[test]
    fn a_frame_ending_mid_header_then_the_rest_arriving() {
        let wire = envelop(b"abcdef");
        let (head, tail) = wire.split_at(3);
        let mut decoder = EnvelopeDecoder::new();
        decoder.feed(head.as_ref());
        assert!(decoder.next().expect("no error").is_none());
        decoder.feed(tail.as_ref());
        assert_eq!(
            decoder.next().expect("no error").expect("envelope").payload,
            Bytes::from_static(b"abcdef")
        );
    }

    #[test]
    fn eof_mid_header_is_unavailable() {
        let mut decoder = EnvelopeDecoder::new();
        decoder.feed(&envelop(b"payload")[..3]);
        decoder.finish();
        let err = decoder.next().expect_err("a truncated header");
        assert!(err.is_unavailable());
        assert_eq!(err.message, "stream closed mid-envelope");
    }

    #[test]
    fn eof_mid_payload_is_unavailable() {
        let wire = envelop(b"a longer payload");
        let mut decoder = EnvelopeDecoder::new();
        decoder.feed(&wire[..9]);
        decoder.finish();
        let err = decoder.next().expect_err("a truncated payload");
        assert!(err.is_unavailable());
        assert_eq!(err.message, "stream closed mid-envelope");
    }

    /// A decoder-layer statement only: a boundary EOF leaves no partial
    /// envelope to complain about. It is NOT a clean close of the stream — the
    /// stream layer reports a boundary EOF without an end-of-stream envelope
    /// as truncation (`tests/connect.rs`).
    #[test]
    fn eof_at_an_envelope_boundary_leaves_no_partial_envelope() {
        let mut decoder = EnvelopeDecoder::new();
        decoder.feed(&envelop(b"payload"));
        decoder.finish();
        assert!(decoder.next().expect("no error").is_some());
        assert!(decoder.next().expect("no error").is_none());
    }

    #[test]
    fn an_oversized_length_is_refused_before_buffering() {
        let mut decoder = EnvelopeDecoder::new();
        let mut header = vec![0u8];
        header.extend_from_slice(&u32::MAX.to_be_bytes());
        decoder.feed(&header);
        let err = decoder.next().expect_err("an oversized envelope");
        assert_eq!(err.code, Code::ResourceExhausted);
    }

    #[test]
    fn an_empty_payload_is_a_real_envelope() {
        let mut decoder = EnvelopeDecoder::new();
        decoder.feed(&envelop(b""));
        let envelope = decoder.next().expect("no error").expect("envelope");
        assert_eq!(envelope.flags, 0);
        assert!(envelope.payload.is_empty());
    }
}
