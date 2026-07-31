# kyc-engine — CPU-only KYC/KYB inference image (SPEC A §1, ~1.9GB)
FROM python:3.12-slim

ENV PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    OMP_NUM_THREADS=4

# OpenCV/Paddle runtime libs
RUN apt-get update && apt-get install -y --no-install-recommends \
    libgl1 libglib2.0-0 libgomp1 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /srv
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY kyc_engine ./kyc_engine
COPY tests ./tests

EXPOSE 8105
# AUTH_MODE=keycloak in prod; dev compose uses AUTH_MODE=dev
CMD ["uvicorn", "kyc_engine.main:app", "--host", "0.0.0.0", "--port", "8105"]
