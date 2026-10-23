// SPDX-License-Identifier: Apache-2.0
//! [`Client::follow_events`]: the event feed, re-opened for the caller.
//!
//! A raw [`Client::stream_events`] ends on every idle window, dropped
//! connection and daemon restart, and resuming it correctly takes a cursor
//! built from every event received plus a replay `floor` for children never
//! seen. This module does that once, so no caller rebuilds it:
//!
//! - It tracks `cursor.ordinals` from the events it yields, and sets `floor`
//!   from the server's clock: the `Date` of its last open, or a later event's
//!   `ts`. The daemon replays a child the cursor does not name from the floor,
//!   so a child born during a gap is not missed.
//! - A quiet stream that reaches its idle window is re-opened at once and
//!   silently: nothing was flowing, so nothing was lost.
//! - Any other end — `unavailable`, a truncated stream, a clean end-of-stream
//!   (a daemon shutting down) — yields [`Followed::Interrupted`] and re-opens
//!   after a jittered exponential backoff. Durable events from the gap are
//!   replayed; ephemeral ones (`content_block_delta`) are gone, and the
//!   `AssistantMessage` that follows carries the full text. A renderer that
//!   holds partial text should drop it on `Interrupted`.
//! - Any other error is terminal: it is yielded once, then the stream ends. A
//!   bad token, a protocol mismatch, an unknown child or a refused subject
//!   cannot be fixed by retrying.

use std::collections::HashMap;
use std::pin::Pin;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use futures_core::Stream;
use futures_util::StreamExt as _;

use crate::{pb, Client, Code, Error, Streaming};

/// How [`Client::follow_events`] waits and retries.
#[derive(Debug, Clone)]
pub struct FollowPolicy {
    /// The idle window of each open (see [`Client::stream_events`]). A stream
    /// quiet for this long is re-opened at once, without a `Interrupted`.
    pub idle: Duration,
    /// The first retry's delay after an interruption; each consecutive one
    /// doubles it, up to `max_backoff`, with ±20% jitter.
    pub initial_backoff: Duration,
    pub max_backoff: Duration,
    /// Consecutive failed attempts allowed before the stream gives up with
    /// the last error. `None` retries forever. An event received resets the
    /// count.
    pub max_retries: Option<u32>,
}

impl Default for FollowPolicy {
    fn default() -> FollowPolicy {
        FollowPolicy {
            idle: Duration::from_secs(30),
            initial_backoff: Duration::from_millis(500),
            max_backoff: Duration::from_secs(30),
            max_retries: None,
        }
    }
}

/// One item of a followed stream.
#[derive(Debug)]
pub enum Followed {
    /// An event, in the order the daemon sent it.
    Event(pb::Event),
    /// The stream broke and will re-open after `retry_in`: ephemeral events
    /// from the gap are lost, durable ones will be replayed. `attempt` counts
    /// consecutive interruptions since the last event.
    Interrupted {
        attempt: u32,
        cause: Error,
        retry_in: Duration,
    },
}

/// The stream [`Client::follow_events`] returns.
pub type Following = Pin<Box<dyn Stream<Item = Result<Followed, Error>> + Send>>;

impl Client {
    /// Follow `req`'s events, re-opening the stream across idle windows,
    /// dropped connections and daemon restarts (see the module docs).
    ///
    /// `req.cursor` seeds the first open; leave it unset to start live. A
    /// live `child` subject first reads the child's `latest_ordinal` and opens
    /// from there — a child subject the cursor does not name replays its whole
    /// log, which a re-open of a live stream must never do.
    pub fn follow_events(&self, req: pb::StreamEventsRequest, policy: FollowPolicy) -> Following {
        let follow = Follow::new(self.clone(), req, policy);
        Box::pin(futures_util::stream::unfold(
            follow,
            |mut follow| async move {
                let item = follow.next().await?;
                Some((item, follow))
            },
        ))
    }
}

struct Follow {
    client: Client,
    /// The caller's request with its cursor taken out: subject, tier, types.
    request: pb::StreamEventsRequest,
    policy: FollowPolicy,
    /// The caller supplied a cursor, so the first open replays from it.
    caller_cursor: bool,
    caller_floor: Option<prost_types::Timestamp>,
    /// child -> highest ordinal yielded (or seeded).
    ordinals: HashMap<String, i32>,
    /// The latest server time this follower is known to have been connected
    /// at: its last open's `Date`, or a later event's `ts`.
    connected: Option<prost_types::Timestamp>,
    stream: Option<Streaming<pb::Event>>,
    /// An open has succeeded, so the cursor is this follower's from now on.
    opened: bool,
    failures: u32,
    delay: Option<Duration>,
    done: bool,
}

impl Follow {
    fn new(client: Client, mut request: pb::StreamEventsRequest, policy: FollowPolicy) -> Follow {
        let cursor = request.cursor.take();
        let caller_cursor = cursor.is_some();
        let (ordinals, caller_floor) = match cursor {
            Some(cursor) => (cursor.ordinals, cursor.floor),
            None => (HashMap::new(), None),
        };
        Follow {
            client,
            request,
            policy,
            caller_cursor,
            caller_floor,
            ordinals,
            connected: None,
            stream: None,
            opened: false,
            failures: 0,
            delay: None,
            done: false,
        }
    }

    async fn next(&mut self) -> Option<Result<Followed, Error>> {
        loop {
            if self.done {
                return None;
            }
            if self.stream.is_none() {
                if let Some(delay) = self.delay.take() {
                    tokio::time::sleep(delay).await;
                }
                match self.open().await {
                    Ok(stream) => {
                        self.opened = true;
                        if let Some(at) = stream.opened_at() {
                            self.advance(prost_types::Timestamp::from(at));
                        }
                        self.stream = Some(stream);
                    }
                    // No headers within the window: a daemon that does not
                    // flush them at open, with nothing to say yet.
                    Err(err) if err.is_idle() => continue,
                    Err(err) => return Some(self.interrupt(err)),
                }
            }
            let stream = self.stream.as_mut()?;
            match stream.next().await {
                Some(Ok(event)) => {
                    self.observe(&event);
                    self.failures = 0;
                    return Some(Ok(Followed::Event(event)));
                }
                Some(Err(err)) if err.is_idle() => self.stream = None,
                Some(Err(err)) => {
                    self.stream = None;
                    return Some(self.interrupt(err));
                }
                None => {
                    self.stream = None;
                    // The daemon closed the stream cleanly: it is shutting
                    // down, and the follower outlives it.
                    return Some(
                        self.interrupt(Error::new(
                            Code::Unavailable,
                            "the daemon ended the stream",
                        )),
                    );
                }
            }
        }
    }

    async fn open(&mut self) -> Result<Streaming<pb::Event>, Error> {
        let cursor = self.cursor().await?;
        let request = pb::StreamEventsRequest {
            cursor,
            ..self.request.clone()
        };
        self.client.stream_events(&request, self.policy.idle).await
    }

    /// The cursor for the next open. Before any open has succeeded it is the
    /// caller's own; after one it names every child yielded so far and floors
    /// replay at the last time this follower was connected.
    async fn cursor(&mut self) -> Result<Option<pb::EventCursor>, Error> {
        if !self.caller_cursor && !self.opened {
            if let Some(child) = self.live_child() {
                if !self.ordinals.contains_key(&child) {
                    let latest = self
                        .client
                        .get_child(&pb::GetChildRequest {
                            child_id: child.clone(),
                        })
                        .await?
                        .child
                        .and_then(|summary| summary.latest_ordinal)
                        .unwrap_or(-1);
                    self.ordinals.insert(child, latest);
                }
            } else {
                return Ok(None);
            }
        }
        Ok(Some(pb::EventCursor {
            ordinals: self.ordinals.clone(),
            floor: self.connected.or(self.caller_floor),
        }))
    }

    /// The subject's child, when the subject is one child.
    fn live_child(&self) -> Option<String> {
        match self.request.subject.as_ref()?.scope.as_ref()? {
            pb::event_subject::Scope::Child(child) => Some(child.clone()),
            _ => None,
        }
    }

    fn observe(&mut self, event: &pb::Event) {
        if let Some(ordinal) = event.ordinal {
            let seen = self
                .ordinals
                .entry(event.child_id.clone())
                .or_insert(ordinal);
            *seen = (*seen).max(ordinal);
        }
        if let Some(ts) = event.ts {
            self.advance(ts);
        }
    }

    fn advance(&mut self, at: prost_types::Timestamp) {
        let later = match self.connected {
            Some(current) => (at.seconds, at.nanos) > (current.seconds, current.nanos),
            None => true,
        };
        if later {
            self.connected = Some(at);
        }
    }

    /// An interruption: retried if the daemon may come back, else terminal.
    fn interrupt(&mut self, cause: Error) -> Result<Followed, Error> {
        if !cause.is_unavailable() {
            self.done = true;
            return Err(cause);
        }
        self.failures += 1;
        if self
            .policy
            .max_retries
            .is_some_and(|max| self.failures > max)
        {
            self.done = true;
            return Err(cause);
        }
        let retry_in = self.backoff();
        self.delay = Some(retry_in);
        Ok(Followed::Interrupted {
            attempt: self.failures,
            cause,
            retry_in,
        })
    }

    fn backoff(&self) -> Duration {
        let doublings = self.failures.saturating_sub(1).min(16);
        let base = self
            .policy
            .initial_backoff
            .saturating_mul(1 << doublings)
            .min(self.policy.max_backoff);
        // ±20% jitter so a fleet of followers does not reconnect in lockstep
        // after a daemon restart. The clock's sub-second noise is plenty.
        let noise = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map(|since| since.subsec_nanos() % 401)
            .unwrap_or(200);
        base.mul_f64(0.8 + f64::from(noise) / 1000.0)
    }
}
