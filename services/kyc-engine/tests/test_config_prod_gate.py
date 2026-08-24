"""B1 finding #9 regression: PROFILE=prod must refuse bundled-sample
sanctions/PEP screening and [SIM] CAC fixtures (fail-closed)."""
import pytest

from kyc_engine.config import Settings


def test_prod_refuses_bundled_sample_screening():
    with pytest.raises(ValueError, match="SCREENING_PROVIDER_URL"):
        Settings(profile="prod")


def test_prod_refuses_sim_cac_even_with_real_screening():
    with pytest.raises(ValueError, match="CAC_REGISTRY_URL"):
        Settings(profile="prod", screening_provider_url="https://screening.example")


def test_prod_ok_with_real_sources():
    s = Settings(profile="prod",
                 screening_provider_url="https://screening.example",
                 cac_registry_url="https://cac.example")
    assert s.profile == "prod"


def test_prod_ok_with_offline_real_list():
    s = Settings(profile="prod",
                 screening_list_path="/var/lib/kyc/sanctions.json",
                 cac_registry_url="https://cac.example")
    assert s.profile == "prod"


def test_dev_keeps_sims():
    s = Settings(profile="dev")
    assert s.screening_provider_url == ""
    assert s.cac_registry_url == ""
