use std::io::{Read, Write};

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let mut input = Vec::new();
    std::io::stdin().read_to_end(&mut input)?;
    let chunk = std::env::args()
        .nth(1)
        .unwrap_or_else(|| "4096".to_string())
        .parse()?;
    if chunk == 0 {
        return Err("chunk must be positive".into());
    }
    let output = match std::env::args().nth(2).as_deref() {
        None => statute_htmlrewrite_spike::native(&input, chunk)?,
        Some("--matcher-probe") => statute_htmlrewrite_spike::native_matcher_probe(&input, chunk)?,
        Some("--program") => {
            let program = std::fs::read(std::env::args().nth(3).ok_or("missing program")?)?;
            let replies = std::fs::read(std::env::args().nth(4).ok_or("missing replies")?)?;
            let mut bytes = replies.as_slice();
            let mut frames = Vec::new();
            while !bytes.is_empty() {
                if bytes.len() < 4 {
                    return Err("truncated reply framing".into());
                }
                let n = u32::from_le_bytes(bytes[..4].try_into()?) as usize;
                bytes = &bytes[4..];
                if n > bytes.len() {
                    return Err("invalid reply framing".into());
                }
                frames.push(bytes[..n].to_vec());
                bytes = &bytes[n..];
            }
            let output =
                statute_htmlrewrite_spike::native_program(&input, chunk, &program, frames)?;
            if let Some(path) = std::env::args().nth(5) {
                std::fs::write(path, statute_htmlrewrite_spike::native_events())?;
            }
            output
        }
        Some(_) => return Err("unknown probe".into()),
    };
    std::io::stdout().write_all(&output)?;
    Ok(())
}
