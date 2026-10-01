from calendar import monthrange
from datetime import datetime, timedelta, timezone


def next_occurrence(value, repeat, zone, after, month_day=None, wall_time=None):
    local = datetime.fromisoformat(value).astimezone(zone)
    if wall_time:
        hour,minute,second = [int(part) for part in wall_time.split(':')]
        local=local.replace(hour=hour,minute=minute,second=second)
    if repeat == 'none':
        return value
    if repeat == 'monthly':
        day = month_day or local.day
        while local.astimezone(timezone.utc) <= after:
            year, month = local.year, local.month + 1
            if month == 13:
                year, month = year + 1, 1
            local = local.replace(year=year, month=month, day=min(day, monthrange(year, month)[1]))
    else:
        step = timedelta(days=7 if repeat == 'weekly' else 1)
        # Jump over long downtime without one iteration per missed occurrence.
        days = (after.astimezone(zone).date() - local.date()).days
        if days > 1:
            local += step * max(0, days // step.days - 1)
        while local.astimezone(timezone.utc) <= after or (repeat == 'weekdays' and local.weekday() >= 5):
            local += step
    # A nonexistent wall time during spring-forward resolves forward to a valid instant.
    local = local.astimezone(timezone.utc).astimezone(zone)
    return local.astimezone(timezone.utc).isoformat()
