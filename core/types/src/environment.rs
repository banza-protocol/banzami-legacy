//! Which financial universe a record belongs to.
//!
//! This lives in the shared types crate because every writer of a financial row
//! needs the same answer, and the alternative — each crate reading the variable
//! for itself — is how five slightly different answers appear.
//!
//! It is not merely a label. `environment` is a filter on proof lookup, wallet
//! payment listing, KYC cases and activation, so a row carrying the wrong value
//! is a row that some queries cannot see and others find by mistake.
//!
//! The database column defaults to 'LIVE', which is a fail-OPEN default: a writer
//! that forgets the column does not fail, it silently claims to be real money.
//! That is exactly what happened — several writers omitted it and every Sandbox
//! transfer, payment link, QR code and payout asserted it was LIVE. The default is
//! being removed; until then, every writer supplies this explicitly.

/// The financial universe this process serves.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Environment {
    Live,
    Sandbox,
}

impl Environment {
    /// Read from the process environment.
    ///
    /// Defaults to Live, which is the safe direction for a *runtime* default even
    /// though it is the dangerous one for a *column* default: a process that does
    /// not know what it is must not quietly serve the Sandbox universe and let
    /// test money mix with real records. A Sandbox deployment says so explicitly.
    pub fn from_env() -> Self {
        match std::env::var("ENVIRONMENT").as_deref() {
            Ok(v) if v.eq_ignore_ascii_case("SANDBOX") => Environment::Sandbox,
            _ => Environment::Live,
        }
    }

    /// The canonical string persisted in financial rows.
    pub fn as_str(self) -> &'static str {
        match self {
            Environment::Live => "LIVE",
            Environment::Sandbox => "SANDBOX",
        }
    }

    pub fn is_live(self) -> bool {
        self == Environment::Live
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn canonical_strings_match_the_column_vocabulary() {
        assert_eq!(Environment::Live.as_str(), "LIVE");
        assert_eq!(Environment::Sandbox.as_str(), "SANDBOX");
    }
}
