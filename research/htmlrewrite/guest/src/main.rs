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
        Some(_) => return Err("unknown probe".into()),
    };
    std::io::stdout().write_all(&output)?;
    Ok(())
}
