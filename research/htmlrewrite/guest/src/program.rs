//! Private, bounded protocol shared by the native oracle and Wasm guest.
use lol_html::html_content::{Comment, ContentType, DocumentEnd, Element, EndTag, TextChunk};
use lol_html::{
    DocumentContentHandlers, ElementContentHandlers, HandlerResult, MatcherEvent, MemorySettings,
    Settings,
};
use std::borrow::Cow;
use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

const ABI: u32 = 1;
type Error = Box<dyn std::error::Error + Send + Sync>;
type Result<T> = std::result::Result<T, Error>;

struct Reader<'a> {
    bytes: &'a [u8],
}
impl<'a> Reader<'a> {
    fn take(&mut self, n: usize) -> Result<&'a [u8]> {
        if n > self.bytes.len() {
            return Err("truncated protocol message".into());
        }
        let (a, b) = self.bytes.split_at(n);
        self.bytes = b;
        Ok(a)
    }
    fn u32(&mut self) -> Result<u32> {
        Ok(u32::from_le_bytes(self.take(4)?.try_into()?))
    }
    fn u64(&mut self) -> Result<u64> {
        Ok(u64::from_le_bytes(self.take(8)?.try_into()?))
    }
    fn flag(&mut self) -> Result<bool> {
        match self.u32()? {
            0 => Ok(false),
            1 => Ok(true),
            _ => Err("invalid protocol boolean".into()),
        }
    }
    fn string(&mut self) -> Result<String> {
        let n = self.u32()? as usize;
        Ok(std::str::from_utf8(self.take(n)?)?.to_owned())
    }
    fn done(&self) -> Result<()> {
        if self.bytes.is_empty() {
            Ok(())
        } else {
            Err("trailing protocol bytes".into())
        }
    }
}
struct Writer {
    bytes: Vec<u8>,
    limit: usize,
}
impl Writer {
    fn bytes(&mut self, b: &[u8]) -> Result<()> {
        if b.len() > self.limit.saturating_sub(self.bytes.len()) {
            return Err("event byte budget exceeded".into());
        }
        self.bytes.extend_from_slice(b);
        Ok(())
    }
    fn u32(&mut self, n: u32) -> Result<()> {
        self.bytes(&n.to_le_bytes())
    }
    fn u64(&mut self, n: u64) -> Result<()> {
        self.bytes(&n.to_le_bytes())
    }
    fn string(&mut self, s: &str) -> Result<()> {
        self.u32(s.len().try_into()?)?;
        self.bytes(s.as_bytes())
    }
}
#[derive(Clone)]
struct Command {
    op: u32,
    html: bool,
    name: String,
    content: String,
}
impl Command {
    fn read(r: &mut Reader<'_>) -> Result<Self> {
        Ok(Self {
            op: r.u32()?,
            html: r.flag()?,
            name: r.string()?,
            content: r.string()?,
        })
    }
    fn validate(&self, kind: u32, can_content: bool) -> Result<()> {
        let valid = match self.op {
            1 | 2 => {
                kind == 1
                    && !self.name.is_empty()
                    && !self.html
                    && !self.name.bytes().any(|b| {
                        matches!(
                            b,
                            b' ' | b'\n' | b'\r' | b'\t' | b'\x0C' | b'/' | b'>' | b'='
                        )
                    })
            }
            3 | 4 | 6 => (1..=4).contains(&kind),
            5 => (1..=3).contains(&kind),
            7 | 9 => kind == 1 && can_content,
            8 => kind == 5 || kind == 1 && can_content,
            _ => false,
        };
        let valid = valid
            && (matches!(self.op, 1 | 2) || self.name.is_empty())
            && (!matches!(self.op, 2 | 6) || self.content.is_empty() && !self.html);
        if valid && !self.name.contains('\0') {
            Ok(())
        } else {
            Err("invalid event operation".into())
        }
    }
    fn content_type(&self) -> ContentType {
        if self.html {
            ContentType::Html
        } else {
            ContentType::Text
        }
    }
}
#[derive(Clone)]
struct Rule {
    selector: String,
    kind: u32,
    callback: bool,
    commands: Vec<Command>,
}
#[derive(Clone)]
struct Limits {
    event: usize,
    reply: usize,
    commands: usize,
    scopes: usize,
    parser: usize,
    events: u32,
}
struct State {
    stream: u64,
    invocation: u64,
    events: u32,
    token: u64,
    element: u64,
    text_unit: u64,
    chunk: u32,
    prior_last: bool,
    scopes: BTreeMap<u64, Vec<u32>>,
    limits: Limits,
    #[cfg(not(target_arch = "wasm32"))]
    replies: Vec<Vec<u8>>,
    #[cfg(not(target_arch = "wasm32"))]
    next_reply: usize,
}
impl State {
    fn boundary(&mut self, kind: u32, last: bool) {
        self.token += 1;
        if kind == 2 {
            if self.prior_last {
                self.text_unit += 1;
                self.chunk = 0;
            } else {
                self.chunk += 1;
            }
            self.prior_last = last;
        } else {
            self.prior_last = true;
        }
    }
}
struct Event {
    kind: u32,
    element: u64,
    can_content: bool,
    removed: bool,
    name: String,
    text: String,
    context: String,
    attrs: Vec<(String, String)>,
}
impl Event {
    fn new(kind: u32) -> Self {
        Self {
            kind,
            element: 0,
            can_content: false,
            removed: false,
            name: String::new(),
            text: String::new(),
            context: String::new(),
            attrs: Vec::new(),
        }
    }
}
fn commands(state: &Arc<Mutex<State>>, id: u32, rule: &Rule, event: Event) -> Result<Vec<Command>> {
    {
        let mut s = state.lock().map_err(|_| "poisoned callback state")?;
        s.events = s.events.checked_add(1).ok_or("event count overflow")?;
        if s.events > s.limits.events {
            return Err("event count exceeded".into());
        }
    }
    for c in &rule.commands {
        c.validate(event.kind, event.can_content)?;
    }
    if !rule.callback {
        return Ok(rule.commands.clone());
    }
    let mut s = state.lock().map_err(|_| "poisoned callback state")?;
    s.invocation = s.invocation.checked_add(1).ok_or("invocation overflow")?;
    let mut w = Writer {
        bytes: Vec::new(),
        limit: s.limits.event,
    };
    w.u32(ABI)?;
    w.u64(s.stream)?;
    w.u64(s.invocation)?;
    w.u64(s.token)?;
    w.u64(event.element)?;
    w.u32(id)?;
    w.u32(event.kind)?;
    let identity = w.bytes.clone();
    w.u64(s.text_unit)?;
    w.u32(s.chunk)?;
    w.u32(u32::from(s.prior_last))?;
    w.u32(u32::from(event.can_content))?;
    w.u32(u32::from(event.removed))?;
    w.string(&event.name)?;
    w.string(&event.text)?;
    w.string(&event.context)?;
    w.u32(event.attrs.len().try_into()?)?;
    for (name, value) in &event.attrs {
        w.string(name)?;
        w.string(value)?;
    }
    let scopes: Vec<_> = s
        .scopes
        .iter()
        .filter_map(|(element, rules)| rules.contains(&id).then_some(*element))
        .collect();
    w.u32(scopes.len().try_into()?)?;
    for scope in scopes {
        w.u64(scope)?;
    }
    let limit = s.limits.reply;
    #[cfg(target_arch = "wasm32")]
    let reply = {
        drop(s);
        let mut reply = vec![0; limit];
        let n = unsafe {
            exchange(
                w.bytes.as_ptr() as u32,
                w.bytes.len() as u32,
                reply.as_mut_ptr() as u32,
                limit as u32,
            )
        } as usize;
        if n == 0 || n > limit {
            return Err("host callback failed or exceeded reply capacity".into());
        }
        reply.truncate(n);
        reply
    };
    #[cfg(not(target_arch = "wasm32"))]
    let reply = {
        EVENTS.with_borrow_mut(|events| events.push(w.bytes.clone()));
        let i = s.next_reply;
        s.next_reply += 1;
        let reply = s
            .replies
            .get(i)
            .ok_or("missing native callback reply")?
            .clone();
        if reply.len() > limit {
            return Err("native reply capacity exceeded".into());
        }
        drop(s);
        reply
    };
    let mut r = Reader { bytes: &reply };
    if r.take(identity.len())? != identity {
        return Err("expired or foreign invocation reply".into());
    }
    let count = r.u32()? as usize;
    let s = state.lock().map_err(|_| "poisoned callback state")?;
    if count.saturating_add(rule.commands.len()) > s.limits.commands || count > r.bytes.len() / 16 {
        return Err("reply command count exceeded".into());
    }
    let mut result = rule.commands.clone();
    for _ in 0..count {
        let c = Command::read(&mut r)?;
        c.validate(event.kind, event.can_content)?;
        result.push(c);
    }
    r.done()?;
    Ok(result)
}
#[cfg(target_arch = "wasm32")]
#[link(wasm_import_module = "sink")]
unsafe extern "C" {
    fn exchange(event: u32, length: u32, reply: u32, capacity: u32) -> u32;
}

fn element_commands(el: &mut Element<'_, '_>, commands: Vec<Command>) -> HandlerResult {
    for c in commands {
        let ct = c.content_type();
        match c.op {
            1 => el.set_attribute(&c.name, &c.content)?,
            2 => el.remove_attribute(&c.name),
            3 => el.before(&c.content, ct),
            4 => el.after(&c.content, ct),
            5 => el.replace(&c.content, ct),
            6 => el.remove(),
            7 => el.prepend(&c.content, ct),
            8 => el.append(&c.content, ct),
            9 => el.set_inner_content(&c.content, ct),
            _ => unreachable!(),
        }
    }
    Ok(())
}
macro_rules! unit_commands {
    ($unit:expr,$commands:expr) => {{
        for c in $commands {
            let ct = c.content_type();
            match c.op {
                3 => $unit.before(&c.content, ct),
                4 => $unit.after(&c.content, ct),
                5 => $unit.replace(&c.content, ct),
                6 => $unit.remove(),
                _ => return Err("invalid unit command".into()),
            }
        }
        Ok(())
    }};
}

pub(crate) fn settings(bytes: &[u8], _replies: Vec<Vec<u8>>) -> Result<Settings<'static, 'static>> {
    if bytes.len() > 65536 {
        return Err("program size exceeded".into());
    }
    let mut r = Reader { bytes };
    if r.u32()? != ABI {
        return Err("unsupported program ABI".into());
    }
    let stream = r.u64()?;
    let limits = Limits {
        event: r.u32()? as usize,
        reply: r.u32()? as usize,
        commands: r.u32()? as usize,
        scopes: r.u32()? as usize,
        parser: r.u32()? as usize,
        events: r.u32()?,
    };
    if stream == 0
        || limits.event < 80
        || limits.reply < 48
        || limits.commands == 0
        || limits.scopes == 0
        || limits.parser == 0
        || limits.events == 0
        || limits.event > 1 << 20
        || limits.reply > 1 << 20
        || limits.scopes > 65536
    {
        return Err("invalid program limits".into());
    }
    let count = r.u32()? as usize;
    if count > 1024 || count > r.bytes.len() / 16 {
        return Err("program rule count exceeded".into());
    }
    let mut rules = Vec::new();
    for _ in 0..count {
        let selector = r.string()?;
        let kind = r.u32()?;
        let callback = r.flag()?;
        let n = r.u32()? as usize;
        if !(1..=5).contains(&kind)
            || callback && n != 0
            || n > limits.commands
            || n > r.bytes.len() / 16
            || kind != 5 && selector.is_empty()
            || kind == 5 && !selector.is_empty()
        {
            return Err("invalid program rule".into());
        }
        let command_start = r.bytes.len();
        let mut commands = Vec::new();
        for _ in 0..n {
            let c = Command::read(&mut r)?;
            c.validate(kind, true)?;
            commands.push(c);
        }
        if command_start - r.bytes.len() > limits.reply - 48 {
            return Err("program command byte limit exceeded".into());
        }
        rules.push(Rule {
            selector,
            kind,
            callback,
            commands,
        });
    }
    r.done()?;
    let state = Arc::new(Mutex::new(State {
        stream,
        invocation: 0,
        events: 0,
        token: 0,
        element: 0,
        text_unit: 0,
        chunk: 0,
        prior_last: true,
        scopes: BTreeMap::new(),
        limits: limits.clone(),
        #[cfg(not(target_arch = "wasm32"))]
        replies: _replies,
        #[cfg(not(target_arch = "wasm32"))]
        next_reply: 0,
    }));
    let rule_ids: Vec<u32> = rules
        .iter()
        .enumerate()
        .filter_map(|(id, r)| (r.kind != 5).then_some(id as u32))
        .collect();
    let observer = Arc::clone(&state);
    let token = Arc::clone(&state);
    let mut settings = Settings::new()
        .with_memory_settings(
            MemorySettings::default().with_max_allowed_memory_usage(limits.parser),
        )
        .with_token_observer(move |kind, last| token.lock().unwrap().boundary(kind, last))
        .with_matcher_observer(move |event| {
            let mut s = observer.lock().unwrap();
            match event {
                MatcherEvent::Match {
                    element,
                    rule,
                    content,
                } => {
                    s.element = element;
                    if content {
                        // Track one extra scope to report overflow in the element handler.
                        let max = s.limits.scopes;
                        if s.scopes.len() <= max {
                            s.scopes
                                .entry(element)
                                .or_default()
                                .push(rule_ids[rule as usize]);
                        }
                    }
                }
                MatcherEvent::Retire { element, .. } => {
                    s.scopes.remove(&element);
                }
            }
        });
    let mut end_rules = Vec::new();
    for (id, rule) in rules.into_iter().enumerate() {
        let id = id as u32;
        let shared = Arc::clone(&state);
        if rule.kind == 5 {
            end_rules.push((id, rule));
            continue;
        }
        let selector = Cow::Owned(rule.selector.parse::<lol_html::Selector>()?);
        let mut handlers = ElementContentHandlers::default();
        // Check scope capacity at element entry for every rule.
        let element_rule = rule.clone();
        let element_state = Arc::clone(&shared);
        handlers = handlers.element(move |el: &mut Element<'_, '_>| {
            let s = element_state.lock().unwrap();
            if s.scopes.len() > s.limits.scopes {
                return Err("scope budget exceeded".into());
            }
            let element = s.element;
            drop(s);
            if element_rule.kind == 1 {
                let mut event = Event::new(1);
                event.element = element;
                event.name = el.tag_name();
                event.can_content = el.can_have_content();
                event.removed = el.removed();
                event.attrs = el
                    .attributes()
                    .iter()
                    .map(|a| (a.name(), a.value()))
                    .collect();
                element_commands(el, commands(&element_state, id, &element_rule, event)?)?;
            }
            if element_rule.kind == 4 && el.can_have_content() {
                let end_state = Arc::clone(&element_state);
                let end_rule = element_rule.clone();
                el.on_end_tag(Box::new(move |end: &mut EndTag<'_>| {
                    let mut event = Event::new(4);
                    event.element = element;
                    event.name = end.name();
                    event.removed = end.removed();
                    for c in commands(&end_state, id, &end_rule, event)? {
                        let ct = c.content_type();
                        match c.op {
                            3 => end.before(&c.content, ct),
                            4 => end.after(&c.content, ct),
                            6 => end.remove(),
                            _ => return Err("invalid end command".into()),
                        }
                    }
                    Ok(())
                }))?;
            }
            Ok(())
        });
        if rule.kind == 2 {
            handlers = handlers.text(move |text: &mut TextChunk<'_>| {
                let mut event = Event::new(2);
                event.text = text.as_str().into();
                event.context = format!("{:?}", text.text_type());
                event.removed = text.removed();
                unit_commands!(text, commands(&shared, id, &rule, event)?)
            });
        } else if rule.kind == 3 {
            handlers = handlers.comments(move |comment: &mut Comment<'_>| {
                let mut event = Event::new(3);
                event.text = comment.text();
                event.removed = comment.removed();
                unit_commands!(comment, commands(&shared, id, &rule, event)?)
            });
        }
        settings = settings.append_element_content_handler((selector, handlers));
    }
    if !end_rules.is_empty() {
        let shared = Arc::clone(&state);
        settings = settings.append_document_content_handler(
            DocumentContentHandlers::default().end(move |end: &mut DocumentEnd<'_>| {
                for (id, rule) in end_rules {
                    for c in commands(&shared, id, &rule, Event::new(5))? {
                        end.append(&c.content, c.content_type());
                    }
                }
                Ok(())
            }),
        );
    }
    Ok(settings)
}

pub fn native_program(
    input: &[u8],
    chunk: usize,
    program: &[u8],
    replies: Vec<Vec<u8>>,
) -> std::result::Result<Vec<u8>, String> {
    super::OUTPUT.with_borrow_mut(Vec::clear);
    #[cfg(not(target_arch = "wasm32"))]
    EVENTS.with_borrow_mut(Vec::clear);
    let settings = settings(program, replies).map_err(|e| e.to_string())?;
    let mut rewriter = lol_html::HtmlRewriter::new(settings, super::output as fn(&[u8]));
    for bytes in input.chunks(chunk) {
        rewriter.write(bytes).map_err(|e| e.to_string())?;
    }
    rewriter.end().map_err(|e| e.to_string())?;
    Ok(super::OUTPUT.with_borrow_mut(std::mem::take))
}

#[cfg(not(target_arch = "wasm32"))]
thread_local! { static EVENTS: std::cell::RefCell<Vec<Vec<u8>>> = const { std::cell::RefCell::new(Vec::new()) }; }
#[cfg(not(target_arch = "wasm32"))]
pub fn native_events() -> Vec<u8> {
    let mut out = Vec::new();
    EVENTS.with_borrow_mut(|events| {
        for event in events.drain(..) {
            out.extend_from_slice(&(event.len() as u32).to_le_bytes());
            out.extend_from_slice(&event);
        }
    });
    out
}
