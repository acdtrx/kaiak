// Real calendar dates and instants: what a schema pattern cannot check. The patterns
// in protocol/schema fix the shape; these check that the date or instant exists.

// The shape is YYYY-MM-DD; checks the day exists.
export function isRealDate(value: string): boolean {
  const [year, month, day] = value.split("-").map(Number);
  if (year === undefined || month === undefined || day === undefined) return false;
  return isRealCalendarDay(year, month, day);
}

// The shape is YYYY-MM-DDTHH:MM:SS[.fraction]Z; checks every field is in range. Leap
// seconds (:60) are rejected.
export function isRealTimestamp(value: string): boolean {
  const [date, time] = value.split("T");
  if (date === undefined || time === undefined || !isRealDate(date)) return false;
  const [hour, minute, second] = time.slice(0, 8).split(":").map(Number);
  if (hour === undefined || minute === undefined || second === undefined) return false;
  return hour <= 23 && minute <= 59 && second <= 59;
}

const DAYS_IN_MONTH = [31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];

// Proleptic Gregorian calendar, as RFC 3339 uses.
function isRealCalendarDay(year: number, month: number, day: number): boolean {
  const monthLength = DAYS_IN_MONTH[month - 1];
  if (monthLength === undefined || day < 1) return false;
  const leap = (year % 4 === 0 && year % 100 !== 0) || year % 400 === 0;
  return day <= (month === 2 && leap ? 29 : monthLength);
}
