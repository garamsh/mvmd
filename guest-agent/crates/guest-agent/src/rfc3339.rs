//! A timestamp as RFC 3339 text in UTC with nanoseconds, the form the console lines carry.

use prost_types::Timestamp;

const SECONDS_PER_DAY: i64 = 86_400;

/// The timestamp as `YYYY-MM-DDTHH:MM:SS.nnnnnnnnnZ`.
pub(crate) fn format_utc(at: &Timestamp) -> String {
    let days = at.seconds.div_euclid(SECONDS_PER_DAY);
    let second_of_day = at.seconds.rem_euclid(SECONDS_PER_DAY);
    let (year, month, day) = civil_from_days(days);
    format!(
        "{year:04}-{month:02}-{day:02}T{:02}:{:02}:{:02}.{:09}Z",
        second_of_day / 3600,
        second_of_day % 3600 / 60,
        second_of_day % 60,
        at.nanos
    )
}

/// The proleptic Gregorian date of a day count from 1970-01-01.
fn civil_from_days(days: i64) -> (i64, i64, i64) {
    let z = days + 719_468;
    let era = z.div_euclid(146_097);
    let day_of_era = z.rem_euclid(146_097);
    let year_of_era =
        (day_of_era - day_of_era / 1_460 + day_of_era / 36_524 - day_of_era / 146_096) / 365;
    let day_of_year = day_of_era - (365 * year_of_era + year_of_era / 4 - year_of_era / 100);
    let shifted_month = (5 * day_of_year + 2) / 153;
    let day = day_of_year - (153 * shifted_month + 2) / 5 + 1;
    let month = if shifted_month < 10 {
        shifted_month + 3
    } else {
        shifted_month - 9
    };
    let year = year_of_era + era * 400 + i64::from(month <= 2);
    (year, month, day)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn at(seconds: i64, nanos: i32) -> Timestamp {
        Timestamp { seconds, nanos }
    }

    #[test]
    fn the_epoch_is_midnight_of_1970() {
        assert_eq!(format_utc(&at(0, 0)), "1970-01-01T00:00:00.000000000Z");
    }

    #[test]
    fn nanoseconds_are_padded_to_nine_digits() {
        assert_eq!(
            format_utc(&at(1_700_000_000, 5_000)),
            "2023-11-14T22:13:20.000005000Z"
        );
    }

    #[test]
    fn a_leap_day_and_a_year_end_keep_their_dates() {
        assert_eq!(
            format_utc(&at(1_709_164_800, 999_999_999)),
            "2024-02-29T00:00:00.999999999Z"
        );
        assert_eq!(
            format_utc(&at(1_767_225_599, 0)),
            "2025-12-31T23:59:59.000000000Z"
        );
    }
}
