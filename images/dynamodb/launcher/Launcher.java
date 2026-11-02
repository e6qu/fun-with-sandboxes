// Launcher starts DynamoDB Local listening on loopback alone. DynamoDB
// Local takes no listen address and binds every interface, which a fwsd
// container may not, so the launcher sets its Jetty connectors' host before
// it starts the server.
import java.lang.reflect.Field;

public class Launcher {
    public static void main(String[] args) throws Exception {
        Class<?> runner = Class.forName("com.amazonaws.services.dynamodbv2.local.main.ServerRunner");
        Object proxy = runner.getMethod("createServerFromCommandLineArgs", String[].class).invoke(null, (Object) args);
        Class<?> jetty = Class.forName("org.eclipse.jetty.server.Server", false, proxy.getClass().getClassLoader());
        Object server = null;
        for (Class<?> c = proxy.getClass(); c != null && server == null; c = c.getSuperclass()) {
            for (Field f : c.getDeclaredFields()) {
                if (jetty.isAssignableFrom(f.getType())) {
                    f.setAccessible(true);
                    server = f.get(proxy);
                    break;
                }
            }
        }
        if (server == null) {
            throw new IllegalStateException("DynamoDB Local holds no Jetty server to bind to loopback");
        }
        Object[] connectors = (Object[]) jetty.getMethod("getConnectors").invoke(server);
        if (connectors.length == 0) {
            throw new IllegalStateException("DynamoDB Local's Jetty server has no connectors");
        }
        for (Object c : connectors) {
            c.getClass().getMethod("setHost", String.class).invoke(c, System.getProperty("fws.host", "127.0.0.1"));
        }
        proxy.getClass().getMethod("start").invoke(proxy);
    }
}
