FROM eclipse-temurin:21.0.5_11-jre
COPY app.jar /app/app.jar
USER 65532:65532
EXPOSE 8080
CMD ["java", "-jar", "/app/app.jar"]
