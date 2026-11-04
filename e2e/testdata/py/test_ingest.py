"""명세(spec.md)에서 독립적으로 도출한 검증. 기대값은 손으로 계산한 값이다."""
import pytest
from ingest import Store, SaveError, ingest, aggregate, save


def rec(source, ts, value):
    return {"source": source, "ts": ts, "value": value}


def test_sum_basic():
    s = Store()
    ingest(s, [rec("a", 1, 10), rec("a", 2, 20), rec("a", 3, 5)])
    assert aggregate(s, 0, 10) == 35


def test_reingest_same_batch_does_not_double():
    s = Store()
    batch = [rec("a", 1, 10), rec("a", 2, 20)]
    ingest(s, batch)
    ingest(s, batch)
    assert aggregate(s, 0, 10) == 30


def test_same_ts_different_source_both_count():
    s = Store()
    ingest(s, [rec("a", 5, 7), rec("b", 5, 3)])
    assert aggregate(s, 0, 10) == 10


def test_missing_is_none_not_zero():
    s = Store()
    ingest(s, [rec("a", 50, 9)])
    assert aggregate(s, 0, 10) is None


def test_start_boundary_included():
    s = Store()
    ingest(s, [rec("a", 10, 4)])
    assert aggregate(s, 10, 20) == 4


def test_end_boundary_excluded():
    s = Store()
    ingest(s, [rec("a", 20, 4)])
    assert aggregate(s, 10, 20) is None


def test_boundary_before_exact_after():
    s = Store()
    ingest(s, [rec("a", 9, 1), rec("a", 10, 2), rec("a", 19, 3), rec("a", 20, 4)])
    assert aggregate(s, 10, 20) == 5


def test_save_is_atomic_on_failure():
    s = Store()
    save(s, {"k0": 1})
    with pytest.raises(SaveError):
        save(s, {"k1": 2, "k2": 3, "k3": 4}, fail_after=2)
    assert s.results == {"k0": 1}


def test_save_success_persists():
    s = Store()
    save(s, {"k1": 2, "k2": 3})
    assert s.results == {"k1": 2, "k2": 3}
