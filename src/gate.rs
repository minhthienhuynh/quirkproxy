//! Classification of upstream free-tier refusals.
//!
//! A `403` from the OpenCode free tier is not one condition: the same status
//! and often the same `FreeTierError` type is returned for at least five
//! unrelated causes, and only some of them are fixed by changing the egress
//! address. Rotating the WARP identity for a request-shape or geo refusal
//! burns ~5s and a fresh registration while changing nothing about the
//! outcome, so the decision to rotate is made from the message, not the status.

/// Why the upstream refused the request.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RefusalKind {
    /// Per-address free usage exhausted. This is the axis a new egress fixes.
    Quota,
    /// Address flagged for abusive traffic. Also fixed by a new egress.
    Abuse,
    /// Request shape is not accepted (headers, tools, session id encoding).
    /// No egress address makes this pass.
    Shape,
    /// Region block. A new WARP registration stays in the same colo.
    Geo,
    /// Credential problem.
    Auth,
    /// Client fingerprint is too old.
    Version,
    /// Unrecognised refusal.
    Unknown,
}

impl RefusalKind {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Quota => "quota",
            Self::Abuse => "abuse",
            Self::Shape => "shape",
            Self::Geo => "geo",
            Self::Auth => "auth",
            Self::Version => "version",
            Self::Unknown => "unknown",
        }
    }

    /// Whether drawing a new egress address can plausibly change the outcome.
    ///
    /// `Unknown` rotates on purpose: an unrecognised 403 on this free tier is
    /// far more often an exhausted address than anything else, and a single
    /// wasted rotation is cheaper than a hard failure. The known
    /// non-egress causes are enumerated above precisely so they are excluded.
    pub fn rotate_eligible(self) -> bool {
        matches!(self, Self::Quota | Self::Abuse | Self::Unknown)
    }

    pub fn advice(self) -> &'static str {
        match self {
            Self::Quota => "quota exhausted for this address -- rotating egress",
            Self::Abuse => "address flagged for abuse -- rotating egress",
            Self::Shape => "request-shape gate -- headers/tools/session id, rotation cannot help",
            Self::Geo => "region blocked -- rotation stays in the same colo",
            Self::Auth => "credential problem, not the free tier",
            Self::Version => "client fingerprint too old -- upstream wants a newer one",
            Self::Unknown => "unrecognised refusal -- upstream may have moved",
        }
    }
}

/// Classifies a `403` response body.
///
/// Matching is on the message text: the upstream types every one of these as
/// `FreeTierError`, so the type carries no information. Needles are checked
/// in a fixed order because the messages overlap (`Abuse` also contains
/// `forbidden`, and a quota message may mention rate limiting).
pub fn classify_403(body: &str) -> RefusalKind {
    let lower = body.to_lowercase();

    if lower.contains("abuse") {
        return RefusalKind::Abuse;
    }
    if lower.contains("freeusagelimiterror")
        || lower.contains("usage limit")
        || lower.contains("rate limit")
        || lower.contains("quota")
    {
        return RefusalKind::Quota;
    }
    if lower.contains("not available in your country") || lower.contains("region") {
        return RefusalKind::Geo;
    }
    // The shape gate phrases it as "model not allowed within opencode" /
    // "... within opencode free".
    if lower.contains("within opencode") || lower.contains("request shape") {
        return RefusalKind::Shape;
    }
    if lower.contains("upgrade required") {
        return RefusalKind::Version;
    }
    if lower.contains("authentication")
        || lower.contains("unauthorized")
        || lower.contains("api key")
    {
        return RefusalKind::Auth;
    }

    RefusalKind::Unknown
}

#[cfg(test)]
mod tests {
    use super::*;

    fn body(err_type: &str, message: &str) -> String {
        format!(r#"{{"type":"error","error":{{"type":"{err_type}","message":"{message}"}}}}"#)
    }

    #[test]
    fn abuse_is_rotation_eligible() {
        let b = body(
            "FreeTierError",
            "Forbidden, OpenCode Free Model Gateway Detect Abuse Traffic",
        );
        assert_eq!(classify_403(&b), RefusalKind::Abuse);
        assert!(classify_403(&b).rotate_eligible());
    }

    #[test]
    fn quota_is_rotation_eligible() {
        let b = body("FreeTierError", "free usage limit reached for this address");
        assert_eq!(classify_403(&b), RefusalKind::Quota);
        assert!(classify_403(&b).rotate_eligible());
    }

    #[test]
    fn rate_limit_wording_counts_as_quota() {
        let b = body("FreeTierError", "rate limit exceeded");
        assert_eq!(classify_403(&b), RefusalKind::Quota);
    }

    #[test]
    fn geo_is_not_rotation_eligible() {
        let b = body("FreeTierError", "not available in your country");
        assert_eq!(classify_403(&b), RefusalKind::Geo);
        assert!(!classify_403(&b).rotate_eligible());
    }

    #[test]
    fn shape_gate_is_not_rotation_eligible() {
        let b = body("FreeTierError", "model not allowed within opencode free");
        assert_eq!(classify_403(&b), RefusalKind::Shape);
        assert!(!classify_403(&b).rotate_eligible());
    }

    #[test]
    fn auth_and_version_are_not_rotation_eligible() {
        assert_eq!(
            classify_403(&body("Error", "invalid api key")),
            RefusalKind::Auth
        );
        assert_eq!(
            classify_403(&body("Error", "upgrade required")),
            RefusalKind::Version
        );
        assert!(!RefusalKind::Auth.rotate_eligible());
        assert!(!RefusalKind::Version.rotate_eligible());
    }

    #[test]
    fn plain_forbidden_without_detail_is_unknown_and_rotates() {
        let kind = classify_403("Forbidden");
        assert_eq!(kind, RefusalKind::Unknown);
        assert!(kind.rotate_eligible());
    }

    #[test]
    fn non_json_body_still_classifies() {
        assert_eq!(
            classify_403("403 Forbidden: free usage limit reached"),
            RefusalKind::Quota
        );
    }
}
