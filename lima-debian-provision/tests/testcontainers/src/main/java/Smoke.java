import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.time.Duration;
import org.testcontainers.containers.GenericContainer;
import org.testcontainers.utility.DockerImageName;

public final class Smoke {
    public static void main(String[] args) throws Exception {
        if (Boolean.parseBoolean(System.getenv("TESTCONTAINERS_RYUK_DISABLED"))) {
            throw new IllegalStateException("Ryuk must be enabled for this smoke test");
        }
        String id;
        try (GenericContainer<?> container = new GenericContainer<>(
                DockerImageName.parse("nginx:alpine")).withExposedPorts(80)) {
            container.start();
            id = container.getContainerId();
            URI uri = URI.create("http://" + container.getHost() + ":"
                    + container.getMappedPort(80) + "/");
            HttpResponse<String> response = HttpClient.newBuilder()
                    .connectTimeout(Duration.ofSeconds(10)).build()
                    .send(HttpRequest.newBuilder(uri).timeout(Duration.ofSeconds(10))
                            .GET().build(), HttpResponse.BodyHandlers.ofString());
            if (response.statusCode() != 200) {
                throw new IllegalStateException("Mapped port returned " + response.statusCode());
            }
            System.out.println("Testcontainers mapped port reachable: " + uri);
        }
        var containers = org.testcontainers.DockerClientFactory.instance().client()
                .listContainersCmd().withShowAll(true).exec();
        if (containers.stream().anyMatch(container -> container.getId().equals(id))) {
            throw new IllegalStateException("Smoke container was not removed");
        }
        System.out.println("Testcontainers smoke passed: Docker API, Ryuk enabled, mapped port, cleanup");
    }
}
