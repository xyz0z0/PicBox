# Resource-test image only. Build the static Linux binary on the host first.
FROM scratch
COPY --chmod=0555 dist/picbox /picbox
ENV PICBOX_ADDR=0.0.0.0:3001 PICBOX_DATA_DIR=/data
USER 65532:65532
EXPOSE 3001
ENTRYPOINT ["/picbox"]
