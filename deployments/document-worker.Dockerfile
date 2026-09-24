FROM python:3.11-slim

ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 \
    WORKER_HOST=0.0.0.0 WORKER_PORT=8091 \
    HF_HOME=/models/huggingface XDG_CACHE_HOME=/models/cache \
    TORCH_HOME=/models/torch OMP_NUM_THREADS=2

RUN apt-get update && apt-get install -y --no-install-recommends \
    libglib2.0-0 libgl1 tesseract-ocr tesseract-ocr-chi-sim tesseract-ocr-eng \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 10001 worker \
    && mkdir /models && chown worker:worker /models

WORKDIR /app
COPY workers/document/requirements.txt ./requirements.txt
# CPU-only PyTorch keeps the first version independent of a CUDA image.
RUN pip install --no-cache-dir torch torchvision --index-url https://download.pytorch.org/whl/cpu \
    && pip install --no-cache-dir -r requirements.txt
COPY workers/document/ /app/
USER worker
EXPOSE 8091
CMD ["python", "worker.py"]
