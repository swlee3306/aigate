"""수집 시스템 예시(설계안 3절): 중복 처리 방지, 결측, 기간 경계, 저장 원자성."""


class SaveError(Exception):
    pass


class Store:
    def __init__(self):
        self.records = []      # (source, ts, value)
        self.seen = set()      # (source, ts)
        self.results = {}      # 저장된 집계 결과


def ingest(store, records):
    """같은 (source, ts)는 한 번만 반영한다. 같은 배치를 다시 처리해도 결과가 늘지 않는다."""
    for r in records:
        key = (r["source"], r["ts"])
        if key in store.seen:
            continue
        store.seen.add(key)
        store.records.append((r["source"], r["ts"], r["value"]))


def aggregate(store, start, end):
    """[start, end) 구간의 합. 구간에 데이터가 없으면 None(결측)이며 0이 아니다."""
    values = [v for (_s, ts, v) in store.records if start <= ts < end]
    if not values:
        return None
    return sum(values)


def save(store, results, fail_after=None):
    """results를 원자적으로 저장한다. 중간에 실패하면 이번 호출의 변경을 모두 되돌리고 SaveError를 낸다."""
    snapshot = dict(store.results)
    try:
        for i, (k, v) in enumerate(results.items()):
            if fail_after is not None and i >= fail_after:
                raise SaveError("injected failure")
            store.results[k] = v
    except SaveError:
        store.results.clear()
        store.results.update(snapshot)
        raise
