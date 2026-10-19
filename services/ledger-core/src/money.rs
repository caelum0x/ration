//! Fixed-point money. Everywhere in this crate, an amount is an `i64`
//! count of minor units (e.g. cents) plus an ISO 4217 currency code.
//! Never float — float drift is unacceptable in a ledger. All arithmetic
//! is checked: overflow and currency mismatches are typed errors, never
//! a panic or silent truncation.

use thiserror::Error;

#[derive(Debug, Error, Clone, PartialEq, Eq)]
pub enum MoneyError {
    #[error("currency mismatch: {a} vs {b}")]
    CurrencyMismatch { a: String, b: String },

    #[error("arithmetic overflow")]
    Overflow,

    #[error("invalid currency code {0:?}: must be a 3-letter ISO 4217 code")]
    InvalidCurrency(String),

    #[error("amount must be non-negative, got {0}")]
    Negative(i64),
}

/// A fixed-point money amount: `minor_units` in `currency`.
///
/// `currency` is always normalized to uppercase ASCII on construction, so
/// `Money::new(100, "usd")` and `Money::new(100, "USD")` compare equal.
#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub struct Money {
    minor_units: i64,
    currency: String,
}

impl Money {
    /// Builds a `Money`, validating the currency code looks like an ISO
    /// 4217 alpha code (exactly 3 ASCII letters). Does not require
    /// `minor_units >= 0` here — negative amounts are valid intermediate
    /// values (e.g. a signed delta); use [`Money::non_negative`] at
    /// boundaries that must reject them.
    pub fn new(minor_units: i64, currency: impl Into<String>) -> Result<Self, MoneyError> {
        let raw = currency.into();
        let normalized = raw.trim().to_uppercase();
        if normalized.len() != 3 || !normalized.chars().all(|c| c.is_ascii_alphabetic()) {
            return Err(MoneyError::InvalidCurrency(raw));
        }
        Ok(Self {
            minor_units,
            currency: normalized,
        })
    }

    /// Like [`Money::new`] but rejects negative amounts — use this when
    /// validating external input (request amounts, deposit amounts).
    pub fn non_negative(minor_units: i64, currency: impl Into<String>) -> Result<Self, MoneyError> {
        let m = Self::new(minor_units, currency)?;
        if m.minor_units < 0 {
            return Err(MoneyError::Negative(m.minor_units));
        }
        Ok(m)
    }

    pub fn zero(currency: impl Into<String>) -> Result<Self, MoneyError> {
        Self::new(0, currency)
    }

    pub fn minor_units(&self) -> i64 {
        self.minor_units
    }

    pub fn currency(&self) -> &str {
        &self.currency
    }

    fn ensure_same_currency(&self, other: &Money) -> Result<(), MoneyError> {
        if self.currency != other.currency {
            return Err(MoneyError::CurrencyMismatch {
                a: self.currency.clone(),
                b: other.currency.clone(),
            });
        }
        Ok(())
    }

    /// Checked addition. Errors on currency mismatch or i64 overflow —
    /// never wraps or panics.
    pub fn checked_add(&self, other: &Money) -> Result<Money, MoneyError> {
        self.ensure_same_currency(other)?;
        let sum = self
            .minor_units
            .checked_add(other.minor_units)
            .ok_or(MoneyError::Overflow)?;
        Ok(Money {
            minor_units: sum,
            currency: self.currency.clone(),
        })
    }

    /// Checked subtraction. Errors on currency mismatch or i64 overflow.
    /// Does NOT reject a negative result (callers enforcing a floor, like
    /// [`crate::ledger`], check that separately — subtraction itself is a
    /// pure arithmetic operation).
    pub fn checked_sub(&self, other: &Money) -> Result<Money, MoneyError> {
        self.ensure_same_currency(other)?;
        let diff = self
            .minor_units
            .checked_sub(other.minor_units)
            .ok_or(MoneyError::Overflow)?;
        Ok(Money {
            minor_units: diff,
            currency: self.currency.clone(),
        })
    }

    pub fn is_same_currency(&self, other: &Money) -> bool {
        self.currency == other.currency
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn normalizes_currency_case() {
        let a = Money::new(100, "usd").unwrap();
        let b = Money::new(100, "USD").unwrap();
        assert_eq!(a.currency(), "USD");
        assert_eq!(a, b);
    }

    #[test]
    fn rejects_invalid_currency() {
        assert!(Money::new(100, "US").is_err());
        assert!(Money::new(100, "USDX").is_err());
        assert!(Money::new(100, "12A").is_err());
    }

    #[test]
    fn checked_add_rejects_currency_mismatch() {
        let usd = Money::new(100, "USD").unwrap();
        let eur = Money::new(100, "EUR").unwrap();
        let err = usd.checked_add(&eur).unwrap_err();
        assert_eq!(
            err,
            MoneyError::CurrencyMismatch {
                a: "USD".to_string(),
                b: "EUR".to_string()
            }
        );
    }

    #[test]
    fn checked_add_detects_overflow() {
        let a = Money::new(i64::MAX, "USD").unwrap();
        let b = Money::new(1, "USD").unwrap();
        assert_eq!(a.checked_add(&b).unwrap_err(), MoneyError::Overflow);
    }

    #[test]
    fn checked_sub_detects_overflow() {
        let a = Money::new(i64::MIN, "USD").unwrap();
        let b = Money::new(1, "USD").unwrap();
        assert_eq!(a.checked_sub(&b).unwrap_err(), MoneyError::Overflow);
    }

    #[test]
    fn non_negative_rejects_negative_amount() {
        assert!(Money::non_negative(-1, "USD").is_err());
        assert!(Money::non_negative(0, "USD").is_ok());
    }
}
