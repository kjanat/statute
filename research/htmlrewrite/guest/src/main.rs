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
    let output = statute_htmlrewrite_spike::native(&input, chunk)?;
    std::io::stdout().write_all(&output)?;
    Ok(())
}
